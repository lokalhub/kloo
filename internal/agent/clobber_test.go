package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// bigSeed is a substantial (> clobberMinBytes) existing-file body, like the real
// ~2 KiB config that was clobbered in the wild.
func bigSeed() string { return strings.Repeat("config: a meaningful line of real content\n", 20) } // ~860 bytes

// TestWriteClobberGuardBlocksBlindShrink: write_file that SHRINKS a substantial file
// the model never read is REFUSED (the file is preserved), the model is nudged to
// read-then-edit, and an INFORMED write after a read is allowed. This is the real
// data-loss footgun: a model blindly overwrote a ~2 KiB config with a ~250-byte stub.
func TestWriteClobberGuardBlocksBlindShrink(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{"path": "config.yaml", "content": "stub\n"}})},                     // BLOCKED (blind shrink)
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "config.yaml"}})},                                           // now informed
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{"path": "config.yaml", "content": "informed and intentional\n"}})}, // ALLOWED (known)
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "updated config"}})},
	)
	loop, root := newRealEditLoop(t, srv, "config.yaml", bigSeed(), nil, &stubBudget{tripAt: 100}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "update the config")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var nudged bool
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "would REPLACE the existing file") {
			nudged = true
		}
	}
	if !nudged {
		t.Error("expected the clobber-guard nudge for the blind shrinking write_file")
	}
	got, err := os.ReadFile(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) == "stub\n" {
		t.Fatal("blind write_file CLOBBERED the file — the guard failed")
	}
	if !strings.Contains(string(got), "informed and intentional") {
		t.Errorf("informed write after read should have applied; got %q", string(got))
	}
}

// TestWriteClobberGuardAllowsNewFile: write_file to a non-existent path is never a
// clobber — it writes straight through with no nudge.
func TestWriteClobberGuardAllowsNewFile(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{"path": "brand_new.txt", "content": "hello\n"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "created file"}})},
	)
	loop, root := newRealEditLoop(t, srv, "seed.txt", "seed\n", nil, &stubBudget{tripAt: 100}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "create a file")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "would REPLACE the existing file") {
			t.Error("clobber guard must NOT fire when writing a new (non-existent) file")
		}
	}
	got, err := os.ReadFile(filepath.Join(root, "brand_new.txt"))
	if err != nil || string(got) != "hello\n" {
		t.Errorf("new file should have been written; got %q err %v", string(got), err)
	}
}

// TestWriteClobberGuardAllowsSmallFile: a small existing file (< clobberMinBytes) is
// cheap to recreate and routinely rewritten, so it is NOT guarded even unread.
func TestWriteClobberGuardAllowsSmallFile(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{"path": "answer.txt", "content": "right\n"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "fixed answer"}})},
	)
	loop, root := newRealEditLoop(t, srv, "answer.txt", "wrong\n", nil, &stubBudget{tripAt: 100}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "fix the answer")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "would REPLACE the existing file") {
			t.Error("clobber guard must NOT fire on a small (< 512 byte) file")
		}
	}
	got, _ := os.ReadFile(filepath.Join(root, "answer.txt"))
	if string(got) != "right\n" {
		t.Errorf("small-file write should have applied; got %q", string(got))
	}
}

// TestWriteClobberGuardAllowsSameOrLargerRewrite: replacing a substantial unread file
// with the SAME-or-MORE content is an additive/full rewrite, not a destructive shrink
// — allowed without a read.
func TestWriteClobberGuardAllowsSameOrLargerRewrite(t *testing.T) {
	bigger := bigSeed() + strings.Repeat("config: an additional appended line\n", 5)
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{"path": "config.yaml", "content": bigger}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "expanded config"}})},
	)
	loop, root := newRealEditLoop(t, srv, "config.yaml", bigSeed(), nil, &stubBudget{tripAt: 100}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "expand the config")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "would REPLACE the existing file") {
			t.Error("a same-or-larger rewrite is not a destructive shrink — must be allowed")
		}
	}
	got, _ := os.ReadFile(filepath.Join(root, "config.yaml"))
	if string(got) != bigger {
		t.Errorf("larger rewrite should have applied")
	}
}
