package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
)

// TestDoctorReportsPatchOnlyAndStopOn: `kloo doctor` surfaces patch_only (A4 DoD)
// and the resolved stop_on rules in both JSON and human output.
func TestDoctorReportsPatchOnlyAndStopOn(t *testing.T) {
	cfg := config.Config{
		Model:      "m",
		Endpoint:   "http://x/v1",
		PatchOnly:  true,
		ScopeAllow: []string{"src/**"},
		StopOn:     config.StopPolicy{OffScopeEdit: true, RepeatedVerify: 2},
	}
	diag := buildResolvedConfigDiagnostic(cfg, "", "", lintOpts{Disabled: true})
	if !diag.PatchOnly {
		t.Fatal("doctor must report patch_only=true")
	}
	if !diag.StopOn.OffScopeEdit || diag.StopOn.RepeatedVerify != 2 {
		t.Fatalf("doctor stop_on = %+v", diag.StopOn)
	}

	var jbuf bytes.Buffer
	if err := writeDoctorJSON(&jbuf, diag); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(jbuf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["patch_only"] != true {
		t.Fatalf("json patch_only = %v", m["patch_only"])
	}

	var hbuf bytes.Buffer
	writeDoctorHuman(&hbuf, diag)
	if !bytes.Contains(hbuf.Bytes(), []byte("patch_only: true")) {
		t.Fatalf("human output missing patch_only line:\n%s", hbuf.String())
	}
	if !bytes.Contains(hbuf.Bytes(), []byte("stop_on:")) {
		t.Fatalf("human output missing stop_on line:\n%s", hbuf.String())
	}
}

// TestDoctorReportsRepeatRounds: doctor answers "what will this run use?" for the
// repetition rail. The knobs have no config-level default (0 ⇒ the agent package
// default, the seam that keeps an unset config building an untuned Loop), so the
// diagnostic must report the EFFECTIVE numbers, not the raw zeros.
func TestDoctorReportsRepeatRounds(t *testing.T) {
	cases := []struct {
		name             string
		cfg              config.Config
		wantNudge        int
		wantAbort        int
		wantHumanContain string
	}{
		{
			name:             "unset reports the effective package defaults",
			cfg:              config.Config{Model: "m", Endpoint: "http://x/v1"},
			wantNudge:        agent.DefaultRepeatNudgeRounds,
			wantAbort:        agent.DefaultRepeatAbortRounds,
			wantHumanContain: "repeat_rounds: nudge=3 abort=6",
		},
		{
			name:             "a tuned abort is reported as resolved",
			cfg:              config.Config{Model: "m", Endpoint: "http://x/v1", RepeatAbortRounds: 20},
			wantNudge:        agent.DefaultRepeatNudgeRounds,
			wantAbort:        20,
			wantHumanContain: "repeat_rounds: nudge=3 abort=20",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diag := buildResolvedConfigDiagnostic(tc.cfg, "", "", lintOpts{Disabled: true})
			if diag.RepeatNudgeRounds != tc.wantNudge || diag.RepeatAbortRounds != tc.wantAbort {
				t.Fatalf("doctor repeat rounds = nudge %d abort %d, want nudge %d abort %d",
					diag.RepeatNudgeRounds, diag.RepeatAbortRounds, tc.wantNudge, tc.wantAbort)
			}

			var jbuf bytes.Buffer
			if err := writeDoctorJSON(&jbuf, diag); err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(jbuf.Bytes(), &m); err != nil {
				t.Fatal(err)
			}
			for key, want := range map[string]int{"repeat_nudge_rounds": tc.wantNudge, "repeat_abort_rounds": tc.wantAbort} {
				got, ok := m[key].(float64)
				if !ok {
					t.Fatalf("json missing %s: %v", key, m[key])
				}
				if int(got) != want {
					t.Errorf("json %s = %d, want %d", key, int(got), want)
				}
			}

			var hbuf bytes.Buffer
			writeDoctorHuman(&hbuf, diag)
			if !bytes.Contains(hbuf.Bytes(), []byte(tc.wantHumanContain)) {
				t.Fatalf("human output missing %q:\n%s", tc.wantHumanContain, hbuf.String())
			}
		})
	}
}

// TestDoctorWorkingSetLineDoesNotClaimToHoldThePrompt.
//
// The line read `working_set: 12000 tokens (BINDING — holds the prompt here instead
// of 73399)` and that was false twice over. The cap lowers the compaction TRIGGER,
// and the trigger bounds only the history compaction can shed — the system prompt,
// the tool schemas and the repo map are re-assembled every turn and no compaction
// reaches them. Measured on kloo's own tree at exactly this configuration before the
// map-budget fix, `kloo context` put the first turn at 26,370 tokens against the
// 12,000 doctor claimed to be holding it to.
//
// It matters because this line is a diagnostic: reporting the cap as working is why
// the map budget sat at 184% of the trigger for three releases with nothing
// complaining. A tool that says a cap is in force has to be right about what the cap
// does.
func TestDoctorWorkingSetLineDoesNotClaimToHoldThePrompt(t *testing.T) {
	cfg := config.Config{
		Model:            "m",
		Endpoint:         "http://x/v1",
		MaxContextTokens: 131072,
		WorkingSetTokens: 12000,
	}
	defer agent.SetWorkingSetTokens(0)
	agent.SetWorkingSetTokens(cfg.WorkingSetTokens)
	diag := buildResolvedConfigDiagnostic(cfg, "", "", lintOpts{Disabled: true})

	var buf bytes.Buffer
	writeDoctorHuman(&buf, diag)
	out := buf.String()

	if !bytes.Contains(buf.Bytes(), []byte("working_set: 12000 tokens (BINDING")) {
		t.Fatalf("the cap is binding at this configuration and must be reported as such:\n%s", out)
	}
	if bytes.Contains(buf.Bytes(), []byte("holds the prompt")) {
		t.Errorf("the working-set line still claims to hold the PROMPT; it holds the compaction trigger:\n%s", out)
	}
	// It must say where compaction starts, what the trigger governs, and where to go
	// for a MEASURED prompt size rather than a derived one.
	for _, want := range []string{"compaction starts here", "HISTORY compaction can shed", "kloo context"} {
		if !bytes.Contains(buf.Bytes(), []byte(want)) {
			t.Errorf("working-set line missing %q:\n%s", want, out)
		}
	}
}
