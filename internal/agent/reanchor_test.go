package agent

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/edit"
	"github.com/lokalhub/kloo/internal/tools"
)

// A STALE ANCHOR IS NOT SPINNING.
//
// Measured on sofronios-legacy, 2026-10-21 (kloo v0.36.3, qwen3.8-flash-next, a
// single-file TypeScript task): the model's first edit was issued before it had
// read the file and failed with edit.ErrSearchNotFound. Re-reading the file is the
// only way to recover from that — a SEARCH/REPLACE block cannot be written without
// the current bytes. The force-edit rail refused every one of those reads:
//
//	→ edit_file: score.ts  [error: edit: SEARCH text not found in file]
//	→ read_file            [error: agent: tool withheld on a forced-edit turn]   ×13
//	→ edit_file: score.ts  [error: edit: SEARCH text not found in file]          ×3 more
//
// The run spent 847,520 tokens and 19m31s over 34 steps, landed no useful edit, and
// stopped BUDGET-EXCEEDED. Its own closing report named the cause: "every subsequent
// read attempt was blocked by the harness, so I could not re-anchor to the real
// contents and could not issue a correct SEARCH/REPLACE."
//
// The rail is right in general — the spin it exists to break is RE-reading a file
// the model has already seen. But a failed SEARCH is positive evidence that the
// model's copy of that file is WRONG, which is the one case where re-reading it is
// the opposite of spinning. The rail already has a precedent for releasing itself
// on proof that forcing cannot work: a scope denial sets editsImpossible and zeroes
// the budget rather than withholding reads for nothing.
func TestForceEditAllowsOneReanchorReadAfterStaleAnchor(t *testing.T) {
	const path = "worker/engine/bot/score.ts"
	read := tools.Call{Name: tools.NameReadFile, Args: map[string]any{"path": path}}

	l := &Loop{Registry: tools.NewRegistry()}
	l.editOnlyLeft = editOnlyBudget

	// Before any failed edit, nothing changes: a re-read of an already-seen file is
	// still the spin the rail is for.
	if l.reanchorRead(read) {
		t.Fatal("a re-read was exempted with no failed edit to justify it")
	}

	// An edit to that path fails on a stale anchor.
	l.noteStaleAnchor(path, edit.ErrSearchNotFound)

	if !l.reanchorRead(read) {
		t.Fatal("the read that recovers from a stale anchor was not exempted")
	}

	// ONE-SHOT. The exemption is spent by the read it allows, so a model that does
	// not act on the contents cannot sit in the exemption and read forever. The
	// worst case is one read, not a loop — the same shape as the read-dedupe
	// pointer's `served` latch.
	if l.reanchorRead(read) {
		t.Fatal("the exemption was granted twice for one failed edit")
	}
}

// The exemption is for the file whose anchor went stale, not a licence to read
// anything. A model that answers a failed edit by reading a DIFFERENT file is
// exploring, and that is what the rail is holding back.
func TestReanchorExemptionIsScopedToTheFailedPath(t *testing.T) {
	l := &Loop{Registry: tools.NewRegistry()}
	l.editOnlyLeft = editOnlyBudget
	l.noteStaleAnchor("a/one.ts", edit.ErrSearchNotFound)

	other := tools.Call{Name: tools.NameReadFile, Args: map[string]any{"path": "a/two.ts"}}
	if l.reanchorRead(other) {
		t.Fatal("a failed edit to one.ts exempted a read of two.ts")
	}
	same := tools.Call{Name: tools.NameReadFile, Args: map[string]any{"path": "a/one.ts"}}
	if !l.reanchorRead(same) {
		t.Fatal("the failed path itself was not exempted")
	}
}

// Only a STALE-ANCHOR failure earns the exemption. An edit refused by the file
// scope, by the verify-path guard or by a churn ban is not evidence that the
// model's copy of the file is wrong, and re-reading cannot help it — those
// refusals have their own handling and must not be turned into a read permit.
func TestOnlyAStaleAnchorEarnsTheExemption(t *testing.T) {
	const path = "a/one.ts"
	read := tools.Call{Name: tools.NameReadFile, Args: map[string]any{"path": path}}
	for name, err := range map[string]error{
		"off-scope":      tools.ErrOffScope,
		"protected path": errProtectedPath,
		"churn-banned":   errChurnBannedPath,
		"nil":            nil,
	} {
		l := &Loop{Registry: tools.NewRegistry()}
		l.editOnlyLeft = editOnlyBudget
		l.noteStaleAnchor(path, err)
		if l.reanchorRead(read) {
			t.Fatalf("%s edit failure was treated as a stale anchor", name)
		}
	}
	// A wrapped ErrSearchNotFound — which is how the tools layer surfaces it — must
	// still be recognised (tools/files.go wraps the engine error with the path).
	l := &Loop{Registry: tools.NewRegistry()}
	l.editOnlyLeft = editOnlyBudget
	l.noteStaleAnchor(path, errors.Join(errors.New("tools: edit_file a/one.ts"), edit.ErrSearchNotFound))
	if !l.reanchorRead(read) {
		t.Fatal("a wrapped ErrSearchNotFound was not recognised")
	}
}

// A SUCCESSFUL edit clears the exemption: the model has re-anchored and acted, and
// the rail's normal rules apply again.
func TestASuccessfulEditClearsTheExemption(t *testing.T) {
	const path = "a/one.ts"
	l := &Loop{Registry: tools.NewRegistry()}
	l.editOnlyLeft = editOnlyBudget
	l.noteStaleAnchor(path, edit.ErrSearchNotFound)
	l.noteStaleAnchor(path, nil) // the next edit to that path succeeded
	if l.reanchorRead(tools.Call{Name: tools.NameReadFile, Args: map[string]any{"path": path}}) {
		t.Fatal("the exemption survived a successful edit to the same path")
	}
}

// The exemption only ever applies to a READ. It must never let a run_command, a
// finish or anything else through a forced-edit turn.
func TestReanchorExemptionAppliesToReadsOnly(t *testing.T) {
	const path = "a/one.ts"
	l := &Loop{Registry: tools.NewRegistry()}
	l.editOnlyLeft = editOnlyBudget
	l.noteStaleAnchor(path, edit.ErrSearchNotFound)
	for _, call := range []tools.Call{
		{Name: tools.NameRunCommand, Args: map[string]any{"command": "cat " + path}},
		{Name: tools.NameFinish, Args: map[string]any{}},
	} {
		if l.reanchorRead(call) {
			t.Fatalf("%s was exempted by a stale anchor", call.Name)
		}
	}
}

// THE WIRING, NOT THE METHOD.
//
// The five tests above pin noteStaleAnchor and reanchorRead in isolation, and an
// implementation can pass every one of them while act() never calls either.
// Measured 2026-10-21: the methods landed, that gate went green, and `grep` found
// no call site anywhere in the loop — a real run behaved exactly as before. A
// method nothing calls is not a fix.
//
// This is a source assertion rather than a behavioural one, deliberately and with
// a known limit: the force-edit rail arms from the run's SECOND no-edit nudge, or
// on a build break at a verify, and a scripted run cannot set those counters
// directly — the first version of this test drove a whole Loop.Run, passed with no
// wiring at all because the rail never armed, and proved nothing. Pinning the two
// call sites is blunt, but it fails for exactly the reason the real run failed.
func TestTheRailActuallyConsultsTheExemption(t *testing.T) {
	src, err := os.ReadFile("loop.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	if !strings.Contains(body, "!l.reanchorRead(call)") {
		t.Error("the force-edit withhold case does not consult reanchorRead: the exemption can never fire")
	}
	if !strings.Contains(body, "l.noteStaleAnchor(") {
		t.Error("nothing ever records a stale anchor: the exemption can never be earned")
	}
}
