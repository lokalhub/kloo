package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/agent"
)

// A task well under the window fits and does not trip the compaction trigger.
func TestMeasureTokensFitsWithHeadroom(t *testing.T) {
	res := measureTokens(strings.Repeat("a", 4000), "arg", 32768)

	if res.ApproxTokens != 1000 {
		t.Fatalf("approx tokens = %d, want 1000 (4 chars/token)", res.ApproxTokens)
	}
	if !res.Fits {
		t.Fatal("expected a 1k-token task to fit a 32k window")
	}
	if res.CompactsAtStart {
		t.Fatal("a 1k-token task must not sit above the compaction trigger")
	}
	if res.UsableWindow != agent.UsableWindow(32768) {
		t.Fatalf("usable window = %d, want the loop's own budget %d",
			res.UsableWindow, agent.UsableWindow(32768))
	}
	if res.Headroom != res.UsableWindow-res.ApproxTokens {
		t.Fatalf("headroom = %d, want %d", res.Headroom, res.UsableWindow-res.ApproxTokens)
	}
}

// The point of the command: a task larger than the usable window is reported as
// not fitting, rather than discovered after the run over-compacts.
func TestMeasureTokensDoesNotFit(t *testing.T) {
	res := measureTokens(strings.Repeat("a", 40000), "arg", 8000)

	if res.Fits {
		t.Fatal("a 10k-token task must not fit an 8k window")
	}
	if !res.CompactsAtStart {
		t.Fatal("a task above the trigger must be reported as compacting at step one")
	}
	if res.Headroom >= 0 {
		t.Fatalf("headroom = %d, want negative when the task overflows", res.Headroom)
	}
}

// Between the compaction trigger and the usable window a task still fits, but
// the loop compacts immediately — both facts have to be reported, not just one.
func TestMeasureTokensFitsButCompactsAtStart(t *testing.T) {
	window := 32768
	trigger := agent.CompactTriggerTokens(window)
	usable := agent.UsableWindow(window)
	chars := (trigger + (usable-trigger)/2) * 4

	res := measureTokens(strings.Repeat("a", chars), "arg", window)

	if !res.Fits {
		t.Fatalf("approx %d should fit usable window %d", res.ApproxTokens, usable)
	}
	if !res.CompactsAtStart {
		t.Fatalf("approx %d is above trigger %d — expected compacts_at_start", res.ApproxTokens, trigger)
	}
}

func TestReadTokensInputRejectsAmbiguousAndEmpty(t *testing.T) {
	if _, _, err := readTokensInput([]string{"task"}, "prompt.md"); err == nil {
		t.Fatal("passing both an argument and --file must be an error, not a silent precedence rule")
	}
	if _, _, err := readTokensInput(nil, ""); err == nil {
		t.Fatal("no argument and no --file must be an error")
	}
}

func TestReadTokensInputFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(path, []byte("build the thing"), 0o644); err != nil {
		t.Fatal(err)
	}

	text, source, err := readTokensInput(nil, path)
	if err != nil {
		t.Fatal(err)
	}
	if text != "build the thing" {
		t.Fatalf("text = %q", text)
	}
	if source != path {
		t.Fatalf("source = %q, want the file path so the output says what was measured", source)
	}
}

// End to end through the real cobra command: --ctx reaches the measurement and
// the JSON shape is what a script would parse.
func TestTokensCommandJSONHonorsCtxFlag(t *testing.T) {
	out, _, err := runCmd(t, Deps{}, "tokens", "--json", "--ctx", "8000", strings.Repeat("a", 4000))
	if err != nil {
		t.Fatalf("tokens --json: %v", err)
	}

	var res tokensResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatalf("tokens emitted invalid JSON: %v\n%s", err, out.String())
	}
	if res.Ctx != 8000 {
		t.Fatalf("ctx = %d, want the --ctx value 8000", res.Ctx)
	}
	if res.ApproxTokens != 1000 {
		t.Fatalf("approx_tokens = %d, want 1000", res.ApproxTokens)
	}
	if res.Source != "arg" {
		t.Fatalf("source = %q, want arg", res.Source)
	}
	if res.Estimate == "" {
		t.Fatal("estimate must name the method so the number is never mistaken for a real tokenizer count")
	}
}

func TestTokensCommandHumanOutput(t *testing.T) {
	out, _, err := runCmd(t, Deps{}, "tokens", "--ctx", "32768", "small task")
	if err != nil {
		t.Fatalf("tokens: %v", err)
	}
	for _, want := range []string{"kloo tokens", "approx_tokens:", "usable_window:", "fits: PASS"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("human output missing %q:\n%s", want, out.String())
		}
	}
}

// The command must never dial a model: no Deps hooks are provided, so any
// attempt to run the loop would fail the test.
func TestTokensCommandDoesNotStartTheLoop(t *testing.T) {
	if _, _, err := runCmd(t, Deps{}, "tokens", "--json", "task"); err != nil {
		t.Fatalf("tokens must resolve config only: %v", err)
	}
}
