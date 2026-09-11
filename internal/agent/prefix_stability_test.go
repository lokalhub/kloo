package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// ─── Prompt-prefix stability harness ────────────────────────────────────────
//
// This is Phase 01's deterministic gate. It drives the loop for several turns
// against the mocked endpoint, captures every outgoing request, and measures how
// much of each turn's prompt is a byte-identical PREFIX of the previous turn's.
//
// Prefix stability is a property of kloo's OWN output, so it needs no provider
// and no network — and unlike a provider's cache-hit rate it cannot be confounded
// by conversation length, which is the confound that made an earlier
// map-placement claim look real when it was not.
//
// Two invariants are the gate (not the ratio itself):
//
//   - APPEND-ONLY — a message sent in turn N is still there, byte-identical, in
//     turn N+1. A message that simply disappears cuts the cache at the point of
//     the deletion. (Catches D5: recentTail's retroactive staleReadDumps removal.)
//   - VOLATILITY ORDER — no message that CHANGES between two turns sits above a
//     message that stays the same. Anything volatile placed early invalidates
//     everything after it (AGENTS.md §Invariants). (Catches D4: the verify pin
//     and the file pin emitted above the transcript tail.)

// regeneratedMarkers identify content kloo SYNTHESIZES fresh every turn by design
// — the two pins and the tail repo map. They are exempt from the append-only
// invariant (they are not transcript, and they are MEANT to change); they are
// emphatically NOT exempt from the volatility-order invariant, which is the one
// that says where they are allowed to sit.
//
// Matched with Contains, not HasPrefix, because the wire prompt is what is
// measured and llm.normalizeMessages merges consecutive same-role messages: a pin
// arrives fused to the task, and the map arrives fused to the last observation.
// That merging is itself part of why a mid-prompt pin is so expensive — it welds
// a volatile string onto an otherwise stable message.
var regeneratedMarkers = []string{
	"Last verify: ",
	"Current file under edit (re-read fresh from disk): ",
	repoMapHeader,
}

func isRegenerated(content string) bool {
	for _, m := range regeneratedMarkers {
		if m != "" && strings.Contains(content, m) {
			return true
		}
	}
	return false
}

// ─── fixture ────────────────────────────────────────────────────────────────

// prefixTurn is one scripted model turn. editPath is the path the turn's call
// puts under edit ("" when the turn does not edit), used only by the fixture
// shape test.
type prefixTurn struct {
	name     string
	call     tcSpec
	editPath string
}

// prefixFixture scripts a conversation that exercises BOTH defects: it changes
// curEditPath twice (a.go at turn 2, b.go at turn 5 — each change is what arms
// staleReadDumps against a DIFFERENT already-sent read dump) and changes the
// verify output once, while running long enough to give ≥ 5 turn pairs.
func prefixFixture() []prefixTurn {
	editA := diffBlock("return a - b\n", "return a + b\n")
	editB := diffBlock("return 0\n", "return 1\n")
	return []prefixTurn{
		{name: "read a", call: tcSpec{"read_file", map[string]any{"path": "a.go"}}},
		{name: "edit a", call: tcSpec{"edit_file", map[string]any{"path": "a.go", "diff": editA}}, editPath: "a.go"},
		{name: "read b", call: tcSpec{"read_file", map[string]any{"path": "b.go"}}},
		{name: "inspect", call: tcSpec{"run_command", map[string]any{"command": "echo stable"}}},
		{name: "edit b", call: tcSpec{"edit_file", map[string]any{"path": "b.go", "diff": editB}}, editPath: "b.go"},
		{name: "re-read a", call: tcSpec{"read_file", map[string]any{"path": "a.go"}}},
		{name: "finish", call: tcSpec{"finish", map[string]any{"summary": "done"}}},
	}
}

// prefixVerifyResults are the scripted verify outcomes, one consumed per turn
// (the last repeats). The output CHANGES exactly once, between the 2nd and 3rd
// verify, so at least one turn pair has a differing verify pin while everything
// else about that pair is stable.
func prefixVerifyResults() []VerifyResult {
	fail := func(out string) VerifyResult {
		return VerifyResult{Command: "go test ./...", ExitCode: 1, Passed: false, Stdout: out}
	}
	return []VerifyResult{
		fail("FAIL: TestAdd want 5 got -1"),
		fail("FAIL: TestAdd want 5 got -1"),
		fail("FAIL: TestOther want 1 got 0"), // the one change
	}
}

// ─── capture ────────────────────────────────────────────────────────────────

// capturedMsg is one prompt message reduced to the bytes that decide cacheability:
// role, content, and the serialized tool_calls.
type capturedMsg struct {
	role    string
	content string
	key     string // role + content + tool_calls, for byte equality
}

// capturePrompts runs the harness and returns the message array of every request
// the loop sent, in order.
func capturePrompts(t *testing.T) [][]capturedMsg {
	t.Helper()
	fixture := prefixFixture()
	mocks := make([]llmtest.Mock, 0, len(fixture))
	for _, turn := range fixture {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 20, turn.call)})
	}
	srv := llmtest.Sequence(t, mocks...)

	loop, _ := newRealEditLoop(t, srv, "a.go", "package main\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n",
		&stubVerifier{results: prefixVerifyResults()}, &stubBudget{tripAt: 50}, &stubChurn{})
	writeFixtureFile(t, loop.Root, "b.go", "package main\n\nfunc Zero() int {\n\treturn 0\n}\n")
	loop.Memory = NewWorkingMemory()
	// Large enough that nothing compacts: this harness measures ORDER, and a
	// compaction legitimately rewrites the middle, which would mask the defects.
	loop.ContextTokens = 1_000_000

	if _, err := loop.Run(context.Background(), "make the tests pass"); err != nil {
		t.Fatalf("harness run: %v", err)
	}

	bodies := srv.ModelCalls()
	if len(bodies) < 6 {
		t.Fatalf("harness captured %d requests, want at least 6", len(bodies))
	}
	out := make([][]capturedMsg, 0, len(bodies))
	for i, b := range bodies {
		var req struct {
			Messages []struct {
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(b, &req); err != nil {
			t.Fatalf("request %d is not JSON: %v", i+1, err)
		}
		msgs := make([]capturedMsg, 0, len(req.Messages))
		for _, m := range req.Messages {
			var content string
			_ = json.Unmarshal(m.Content, &content) // "" for a null/absent content
			msgs = append(msgs, capturedMsg{
				role:    m.Role,
				content: content,
				key:     m.Role + "\x00" + string(m.Content) + "\x00" + string(m.ToolCalls),
			})
		}
		out = append(out, msgs)
	}
	return out
}

// ─── metric ─────────────────────────────────────────────────────────────────

type prefixPair struct {
	a, b      int // 1-based turn numbers
	stable    int // leading byte-identical messages
	total     int // len(turn a's messages)
	ratio     float64
	firstDiff int    // index of the first differing message (-1 ⇒ none in range)
	firstRole string // its role
}

func measurePrefixPairs(prompts [][]capturedMsg) []prefixPair {
	pairs := make([]prefixPair, 0, len(prompts)-1)
	for i := 0; i+1 < len(prompts); i++ {
		a, b := prompts[i], prompts[i+1]
		n := min(len(a), len(b))
		stable := 0
		for stable < n && a[stable].key == b[stable].key {
			stable++
		}
		p := prefixPair{a: i + 1, b: i + 2, stable: stable, total: len(a), firstDiff: -1}
		if len(a) > 0 {
			p.ratio = float64(stable) / float64(len(a))
		}
		if stable < n {
			p.firstDiff, p.firstRole = stable, a[stable].role
		}
		pairs = append(pairs, p)
	}
	return pairs
}

// reportPrefixStability renders the metric. It writes to STDOUT, not t.Log,
// because the report is captured verbatim into the phase artifacts and the
// acceptance greps are line-anchored (`^minRatio: `) — t.Log indents.
func reportPrefixStability(pairs []prefixPair) string {
	var b strings.Builder
	minRatio := 1.0
	for _, p := range pairs {
		where := "none"
		if p.firstDiff >= 0 {
			where = fmt.Sprintf("%d] role=%s", p.firstDiff, p.firstRole)
		} else {
			where = "none]"
		}
		fmt.Fprintf(&b, "pair %d: ratio=%.2f stable=%d/%d first difference at message[%s\n",
			p.a, p.ratio, p.stable, p.total, where)
		if p.ratio < minRatio {
			minRatio = p.ratio
		}
	}
	fmt.Fprintf(&b, "minRatio: %.2f\n", minRatio)
	return b.String()
}

// ─── the gate ───────────────────────────────────────────────────────────────

// TestPromptPrefixAppendOnly: a message sent in turn N is still present,
// byte-identical and in the same relative order, in turn N+1 — so the provider's
// prefix cache is never cut by a retroactive deletion. The two synthesized pins
// and the tail map are exempt (they are regenerated by design); everything that
// came from the transcript is not.
//
// PROOF-OF-DEFECT: this MUST FAIL at v0.16.7 and MUST PASS after Phase 01 Task 03.
// The captured red run is Task 02's deliverable
// (tasks/02-prefix-stability-harness/artifacts/prefix-baseline.txt).
func TestPromptPrefixAppendOnly(t *testing.T) {
	prompts := capturePrompts(t)
	pairs := measurePrefixPairs(prompts)
	fmt.Print(reportPrefixStability(pairs))

	for i := 0; i+1 < len(prompts); i++ {
		a, b := prompts[i], prompts[i+1]
		cursor := 0
		for _, m := range a {
			if isRegenerated(m.content) {
				continue
			}
			found := -1
			for j := cursor; j < len(b); j++ {
				if b[j].key == m.key {
					found = j
					break
				}
			}
			if found < 0 {
				t.Errorf("turn %d → %d: a sent message DISAPPEARED — the prefix is not append-only, so the "+
					"provider's cache is cut here.\n  role=%s content=%.70q tool_calls=%.120s",
					i+1, i+2, m.role, m.content, toolCallsOf(m.key))
				break
			}
			cursor = found + 1
		}
	}
}

// TestPromptPrefixVolatilityOrder: no message that DIFFERS between two turns sits
// above a message that is identical between them. A volatile message early in the
// prompt invalidates every cached token after it, so the order must run stable →
// volatile (AGENTS.md §Invariants). The running-summary slot is excepted: it
// changes only on a compaction, and this fixture never compacts.
//
// PROOF-OF-DEFECT: MUST FAIL at v0.16.7, MUST PASS after Phase 01 Task 03.
func TestPromptPrefixVolatilityOrder(t *testing.T) {
	prompts := capturePrompts(t)

	for i := 0; i+1 < len(prompts); i++ {
		a, b := prompts[i], prompts[i+1]
		n := min(len(a), len(b))
		firstDiff := -1
		for j := 0; j < n; j++ {
			if a[j].key != b[j].key {
				firstDiff = j
				break
			}
		}
		if firstDiff < 0 {
			continue
		}
		for j := firstDiff + 1; j < n; j++ {
			if a[j].key == b[j].key {
				t.Errorf("turn %d → %d: message[%d] (role=%s) CHANGES but message[%d] (role=%s) is stable — "+
					"the volatile message sits above %d stable message(s) and invalidates them:\n  volatile: %.90q\n  stable:   %.90q",
					i+1, i+2, firstDiff, a[firstDiff].role, j, a[j].role, n-j, a[firstDiff].content, a[j].content)
				break
			}
		}
	}
}

// TestPrefixStabilityMetricIsDeterministic: the harness produces identical
// numbers on repeated runs over the same fixture, so the metric can be compared
// across commits. Without this the before/after comparison means nothing.
func TestPrefixStabilityMetricIsDeterministic(t *testing.T) {
	first := reportPrefixStability(measurePrefixPairs(capturePrompts(t)))
	second := reportPrefixStability(measurePrefixPairs(capturePrompts(t)))
	if first != second {
		t.Errorf("metric is not deterministic:\n--- run 1 ---\n%s\n--- run 2 ---\n%s", first, second)
	}
	if !strings.Contains(first, "minRatio: ") {
		t.Errorf("report carries no minRatio line:\n%s", first)
	}
}

// TestPrefixStabilityFixtureShape: the fixture itself keeps the properties the
// harness depends on, so a later edit to it cannot silently weaken the gate.
func TestPrefixStabilityFixtureShape(t *testing.T) {
	fixture := prefixFixture()
	if len(fixture) < 6 {
		t.Errorf("fixture has %d turns, want at least 6", len(fixture))
	}

	edits, last := 0, ""
	for _, turn := range fixture {
		if turn.editPath != "" && turn.editPath != last {
			edits++
			last = turn.editPath
		}
	}
	if edits < 1 {
		t.Errorf("fixture changes curEditPath %d times, want at least 1 (that is what arms staleReadDumps)", edits)
	}

	results := prefixVerifyResults()
	changes := 0
	for i := 1; i < len(results); i++ {
		if results[i].Stdout != results[i-1].Stdout {
			changes++
		}
	}
	if changes < 1 {
		t.Errorf("fixture changes the verify output %d times, want at least 1", changes)
	}
}

// toolCallsOf pulls the serialized tool_calls back out of a capturedMsg key, so a
// disappearing assistant message (whose content is empty — the payload is the tool
// call) names what actually vanished.
func toolCallsOf(key string) string {
	parts := strings.Split(key, "\x00")
	if len(parts) < 3 {
		return ""
	}
	return parts[2]
}

// writeFixtureFile seeds an extra file into the harness workspace.
func writeFixtureFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
