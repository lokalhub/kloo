package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// TestUnsupportedFinishClaim covers the detector in isolation: what counts as a
// claim kloo's tools cannot back, and — just as important — what does not. The
// false-positive rows are the ones that matter; each costs a real run an extra step.
func TestUnsupportedFinishClaim(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "exists.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Root: root}

	cases := []struct {
		name       string
		summary    string
		wrote      map[string]bool
		ranCommand bool
		want       string // substring of the complaint, "" = claim accepted
	}{
		// ── the live incident ────────────────────────────────────────────────
		{
			name:    "created a file that is not there",
			summary: "I've written the reply to reply.md and created sent.txt with reported.",
			wrote:   map[string]bool{"reply.md": true},
			want:    "sent.txt",
		},
		{
			name:       "claims a command it never ran",
			summary:    "Wrote the reply and I ran the curl to post it.",
			wrote:      map[string]bool{"reply.md": true},
			ranCommand: false,
			want:       "run_command never executed",
		},
		// ── must NOT fire ────────────────────────────────────────────────────
		{
			name:    "the file really is on disk",
			summary: "Created exists.txt with the contents.",
			want:    "",
		},
		{
			name:  "a path the run actually wrote, spelled differently",
			wrote: map[string]bool{"docs/notes.md": true},
			// prose names the base, the tool landed the full path
			summary: "I wrote notes.md with the findings.",
			want:    "",
		},
		{
			name:       "ran a command and really did",
			summary:    "I ran the test suite and it passed.",
			ranCommand: true,
			want:       "",
		},
		{
			name:    "ran into a problem is not a claim of acting",
			summary: "I ran into a permissions error and stopped before writing anything.",
			want:    "",
		},
		{
			name:    "a file named as ABSENT",
			summary: "I wrote the summary inline; there is no config.yaml in this repo to update.",
			want:    "",
		},
		{
			name:    "no creation verb at all, just a mention",
			summary: "x.txt is empty, so there was nothing to report.",
			want:    "",
		},
		{
			name:    "a version number is not a filename",
			summary: "Created the release notes for v0.28.0 and 1.5 in exists.txt.",
			want:    "",
		},
		{
			name:    "a hostname is not a filename",
			summary: "Wrote the config pointing at api.example.com for the endpoint.",
			want:    "",
		},
		{
			name:    "empty summary",
			summary: "",
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := l.unsupportedFinishClaim(tc.summary, tc.wrote, tc.ranCommand)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("claim accepted?\nsummary: %q\ngot complaint: %q", tc.summary, got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("complaint = %q, want it to mention %q", got, tc.want)
			}
		})
	}
}

// TestClaimCheckRunsWithoutRoot: with no workspace root the file check cannot run,
// and it must fail OPEN rather than block every finish.
func TestClaimCheckRunsWithoutRoot(t *testing.T) {
	l := &Loop{}
	if got := l.unsupportedFinishClaim("Created sent.txt.", nil, true); got != "" {
		t.Fatalf("no Root should mean no file check, got %q", got)
	}
}

// TestFinishWithUnsupportedClaimIsRefusedOnce drives the real loop through the live
// failure: write a file, then call finish claiming a second file was created that
// never was. kloo must refuse that finish, hand the model the gap, and let it do the
// missing action — and the false sentence must never reach the report.
func TestFinishWithUnsupportedClaimIsRefusedOnce(t *testing.T) {
	root := t.TempDir()
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"edit_file", map[string]any{"path": "reply.md"}})},
		// the false finish
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "Wrote reply.md and created sent.txt with reported."}})},
		// after the corrective: actually act
		// "path" is there because the test registry's recorder requires it (newLoop);
		// the loop only cares that run_command dispatched without error.
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"run_command", map[string]any{"command": "echo reported > sent.txt", "path": "sent.txt"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "Wrote reply.md and ran the command."}})},
	)
	// No verifier on purpose: with one, the mid-loop `lastVerify.Passed && edited`
	// gate (loop.go, defect A) ends the run at the first green-verified edit and the
	// finish under test is never reached. Unverified mode is also the configuration
	// the live incident happened in.
	loop, calls := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.Root = root

	rep, err := loop.Run(context.Background(), "write reply.md then run the command")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonUnverified {
		t.Fatalf("reason = %q, want unverified", rep.Reason)
	}
	if n := rep.RailFires[string(RailFinishClaimUnsupported)]; n != 1 {
		t.Fatalf("rail fires = %d, want 1 (the false finish refused exactly once)", n)
	}
	if strings.Contains(rep.Summary, "created sent.txt") {
		t.Errorf("the refused claim leaked into the report: %q", rep.Summary)
	}
	var ran bool
	for _, c := range *calls {
		if c.Name == tools.NameRunCommand {
			ran = true
		}
	}
	if !ran {
		t.Errorf("the corrective should have produced the missing action, calls = %v", *calls)
	}
}

// TestFinishClaimRefusalIsOneShot: a model that repeats the same unsupported claim
// is NOT fought forever — the second finish stands, so a false positive costs one
// step and never a hung run.
func TestFinishClaimRefusalIsOneShot(t *testing.T) {
	root := t.TempDir()
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "Created sent.txt."}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "Created sent.txt."}})},
	)
	loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 50}, &stubChurn{})
	loop.Root = root

	rep, err := loop.Run(context.Background(), "create sent.txt")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonUnverified {
		t.Fatalf("reason = %q, want unverified (no verifier, second finish honoured)", rep.Reason)
	}
	if n := rep.RailFires[string(RailFinishClaimUnsupported)]; n != 1 {
		t.Fatalf("rail fires = %d, want exactly 1 (one-shot)", n)
	}
	if n := len(srv.Requests()); n != 2 {
		t.Fatalf("requests = %d, want 2 (refused once, then accepted)", n)
	}
}
