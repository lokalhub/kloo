package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// gaugeFixture is a tiny real workspace, so the repo map is non-empty and the gauge
// has something to attribute.
func gaugeFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		"main.go":   "package main\n\nfunc main() { helper() }\n",
		"helper.go": "package main\n\nfunc helper() int { return 41 }\n",
		"README.md": "# fixture\n\nA fixture workspace for the context gauge.\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)
	return root
}

// gaugeJSON runs `kloo context --json` and returns the gauge object.
func gaugeJSON(t *testing.T, args ...string) (map[string]any, map[string]any) {
	t.Helper()
	out, _, err := runCmd(t, Deps{}, append([]string{"context", "--json"}, args...)...)
	if err != nil {
		t.Fatalf("kloo context: %v\n%s", err, out.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	g, ok := raw["gauge"].(map[string]any)
	if !ok {
		t.Fatalf("no gauge object in %s", out.String())
	}
	return g, raw
}

// TestContextCommandReportsEveryConsumerOfTheWindow is the end-to-end claim.
//
// `kloo tokens "x"` reports headroom = usable - approx("x"): the task string alone.
// On a real workspace that omits the system prompt, AGENTS.md, the tool schemas and
// the repo map — essentially the whole of the first turn. Each must now appear with a
// nonzero count, and they must close against the measured total.
func TestContextCommandReportsEveryConsumerOfTheWindow(t *testing.T) {
	gaugeFixture(t)
	g, raw := gaugeJSON(t, "--ctx", "131072", "--model", "m", "make helper return 42")

	num := func(k string) int {
		v, ok := g[k].(float64)
		if !ok {
			t.Fatalf("gauge.%s missing; keys present: %v", k, keysOf(g))
		}
		return int(v)
	}
	for _, k := range []string{"system_tokens", "schema_tokens", "repo_map_tokens", "pinned_hot_tokens"} {
		if num(k) <= 0 {
			t.Errorf("gauge.%s = 0; this consumer of the window is unaccounted for", k)
		}
	}
	sum := num("system_tokens") + num("schema_tokens") + num("repo_map_tokens") +
		num("pinned_hot_tokens") + num("history_tokens") + num("unattributed")
	if used := num("used"); used != sum {
		t.Errorf("sections sum to %d but used = %d", sum, used)
	}
	if num("window") != 131072 {
		t.Errorf("window = %d, want 131072", num("window"))
	}
	// The repo map must dwarf the task. This is the number `headroom` was blind to:
	// it reported ~104,848 free on this window while the map alone spends ~22k.
	if num("repo_map_tokens") <= num("pinned_hot_tokens") {
		t.Errorf("repo map %d should exceed the task %d on a real workspace",
			num("repo_map_tokens"), num("pinned_hot_tokens"))
	}
	// Both kinds of room, and the budget in force named.
	if _, ok := g["free_to_compaction"]; !ok {
		t.Error("free_to_compaction missing — room before compaction is what headroom got wrong by 3.8x")
	}
	if b, _ := g["binding"].(string); b != "working-set" && b != "trigger-fraction" {
		t.Errorf("binding = %q, want the name of the budget in force", b)
	}
	// The estimate must be qualified, as `kloo tokens` qualifies its own.
	notes, _ := raw["notes"].([]any)
	var joined strings.Builder
	for _, n := range notes {
		s, _ := n.(string)
		joined.WriteString(s + "\n")
	}
	for _, want := range []string{"chars/token", "MCP"} {
		if !strings.Contains(joined.String(), want) {
			t.Errorf("notes must mention %q; got:\n%s", want, joined.String())
		}
	}
}

// TestContextCommandNamesTheBindingWindowEndToEnd: at --ctx 600000 the absolute
// working-set cap is ~3.8x tighter than the trigger fraction, and `kloo tokens`
// reported room against the looser one. The command must distinguish them.
func TestContextCommandNamesTheBindingWindowEndToEnd(t *testing.T) {
	gaugeFixture(t)
	g, _ := gaugeJSON(t, "--ctx", "600000", "--model", "m", "x")
	usable, _ := g["usable"].(float64)
	trigger, _ := g["compact_trigger"].(float64)
	free, _ := g["free"].(float64)
	toCompact, _ := g["free_to_compaction"].(float64)
	binding, _ := g["binding"].(string)

	if binding != "working-set" {
		t.Errorf("binding = %q, want working-set at --ctx 600000", binding)
	}
	if trigger >= usable {
		t.Fatalf("trigger %v should be far below usable %v", trigger, usable)
	}
	if free <= toCompact {
		t.Errorf("free (%v) must exceed free-to-compaction (%v) when the working set binds", free, toCompact)
	}
	// And the human output has to show both, or the distinction is academic.
	out, _, err := runCmd(t, Deps{}, "context", "--ctx", "600000", "--model", "m", "x")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"compaction at", "working-set binding", "before compaction"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("human output missing %q:\n%s", want, out.String())
		}
	}
}

// TestContextCommandWritesNothing pins the read-only contract it shares with
// `kloo tokens` and `kloo doctor`.
func TestContextCommandWritesNothing(t *testing.T) {
	root := gaugeFixture(t)
	before := treeOf(t, root)

	out, _, err := runCmd(t, Deps{}, "context", "--ctx", "8000", "--model", "m", "x")
	if err != nil {
		t.Fatalf("kloo context: %v\n%s", err, out.String())
	}
	if after := treeOf(t, root); after != before {
		t.Errorf("the workspace changed:\nbefore %s\nafter  %s", before, after)
	}
	if !strings.Contains(out.String(), "kloo context") {
		t.Errorf("unexpected output:\n%s", out.String())
	}
}

// TestContextCommandHandlesNoWindow: `kloo tokens --ctx 0` prints
// `usable_window: 0, headroom: -1, fits: false` — arithmetic on a missing number,
// presented as a finding. This must say there is no window instead.
func TestContextCommandHandlesNoWindow(t *testing.T) {
	gaugeFixture(t)
	out, _, err := runCmd(t, Deps{}, "context", "--ctx", "0", "--model", "m", "x")
	if err != nil {
		t.Fatalf("kloo context --ctx 0: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "no window configured") {
		t.Errorf("--ctx 0 must say there is no window to measure against:\n%s", out.String())
	}
	if strings.Contains(out.String(), "free              -") {
		t.Errorf("--ctx 0 must not report negative free space:\n%s", out.String())
	}
}

// TestDoctorReportsTheMemoryCeiling: a guard that can stop a run has to be
// inspectable before the run, and the line must state the REAL default — this command
// has a history of describing a rail as off when it shipped on.
func TestDoctorReportsTheMemoryCeiling(t *testing.T) {
	t.Chdir(t.TempDir())

	on, _, err := runCmd(t, Deps{}, "doctor", "--model", "m")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, on.String())
	}
	for _, want := range []string{"memory_guard: ceiling ", "source=default", "map_content_budget: "} {
		if !strings.Contains(on.String(), want) {
			t.Errorf("doctor output missing %q:\n%s", want, on.String())
		}
	}

	t.Setenv("KLOO_MEM_CEILING_MB", "off")
	off, _, err := runCmd(t, Deps{}, "doctor", "--model", "m")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, off.String())
	}
	if !strings.Contains(off.String(), "memory_guard: DISABLED") {
		t.Errorf("a disabled guard must say so:\n%s", off.String())
	}

	t.Setenv("KLOO_MEM_CEILING_MB", "")
	js, _, err := runCmd(t, Deps{}, "doctor", "--json", "--model", "m")
	if err != nil {
		t.Fatalf("doctor --json: %v\n%s", err, js.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(js.Bytes(), &raw); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	mg, ok := raw["memory_guard"].(map[string]any)
	if !ok {
		t.Fatalf("doctor --json has no memory_guard: %v", keysOf(raw))
	}
	for _, k := range []string{"ceiling_bytes", "ceiling_source", "rss_bytes", "map_content_budget_bytes"} {
		if _, ok := mg[k]; !ok {
			t.Errorf("memory_guard.%s missing", k)
		}
	}
	if v, _ := mg["ceiling_bytes"].(float64); v <= 0 {
		t.Errorf("ceiling_bytes = %v; the guard must be ON by default", mg["ceiling_bytes"])
	}
	if v, _ := mg["rss_bytes"].(float64); v <= 0 {
		t.Errorf("rss_bytes = %v; doctor should report the resident set now", mg["rss_bytes"])
	}

	// `kloo tokens`' contract is unchanged: headroom keeps its old (narrow) meaning,
	// and the new accounting lives in a new command rather than redefining a key a
	// script may already read.
	tk, _, err := runCmd(t, Deps{}, "tokens", "--json", "--ctx", "32768", "task")
	if err != nil {
		t.Fatal(err)
	}
	var tj map[string]any
	if err := json.Unmarshal(tk.Bytes(), &tj); err != nil {
		t.Fatal(err)
	}
	if _, ok := tj["headroom"]; !ok {
		t.Error("kloo tokens lost its headroom key")
	}
	if _, ok := tj["used"]; ok {
		t.Error("kloo tokens gained a gauge key — the new meaning belongs in kloo context")
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// treeOf is a cheap fingerprint of a directory's files and sizes, to prove a
// read-only command wrote nothing.
func treeOf(t *testing.T, root string) string {
	t.Helper()
	var lines []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		lines = append(lines, rel+":"+itoa(info.Size()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, " ")
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
