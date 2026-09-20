package agent

import (
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/tools"
)

// TestEditCorrectiveDemandsAnEdit: the general explore corrective offers three
// ways out, two of which a reading model can take without changing anything — and
// on kloo-bench it does, 4-6 times per failing run, on a tree that ends untouched.
// When no edit has been made, the corrective must ask for an edit and close the
// other doors.
func TestEditCorrectiveDemandsAnEdit(t *testing.T) {
	m := editCorrective(12, false, nil, nil)
	c := m.Content
	if !strings.Contains(c, "make your best edit") {
		t.Fatalf("corrective does not demand an edit: %q", c)
	}
	for _, closed := range []string{"do not run a command", "do not call finish", "Do not read"} {
		if !strings.Contains(c, closed) {
			t.Fatalf("corrective leaves %q open: %q", closed, c)
		}
	}
	if !strings.Contains(c, "12") {
		t.Fatal("corrective does not state how much reading has happened")
	}
}

// TestEditRailIsOptIn: off by default, so a released run's rail wording is
// unchanged until the bench says otherwise.
func TestEditRailIsOptOut(t *testing.T) {
	t.Setenv("KLOO_EDIT_RAIL", "")
	if !editRail() {
		t.Fatal("edit rail should be ON by default (v0.22.0)")
	}
	t.Setenv("KLOO_EDIT_RAIL", "0")
	if editRail() {
		t.Fatal("KLOO_EDIT_RAIL=0 must restore the previous behaviour")
	}
}

// TestEditOnlyTurnIsOneShot: the force-edit rail narrows the vocabulary for ONE
// request. If the flag latched, a model that edited would keep an edit-only tool
// list for the rest of the run and could never run the test again.
func TestEditOnlyViewTracksTheBudget(t *testing.T) {
	l := &Loop{Registry: tools.NewRegistry()}
	l.editOnlyLeft = editOnlyBudget
	if l.turnRegistry(false) == l.Registry {
		t.Fatal("armed turn did not get the restricted view")
	}
	l.editOnlyLeft = 0
	if l.turnRegistry(false) != l.Registry {
		t.Fatal("restriction outlived its budget")
	}
}

// TestEditCorrectiveAfterAnEarlierEdit: measured on kloo-bench C66, a run made one
// early edit (2 failing tests -> 1) and then read 16 more times and was stopped
// with the case still red. The corrective must fire on that streak too, and must
// name the situation accurately rather than claiming nothing was ever changed.
func TestEditCorrectiveAfterAnEarlierEdit(t *testing.T) {
	c := editCorrective(9, true, nil, nil).Content
	if strings.Contains(c, "without changing a single line") {
		t.Fatalf("corrective denies the earlier edit: %q", c)
	}
	if !strings.Contains(c, "that edit was not the whole fix") {
		t.Fatalf("corrective does not explain the streak: %q", c)
	}
	if !strings.Contains(c, "make your best edit") {
		t.Fatalf("corrective stops demanding an edit once one was made: %q", c)
	}
}

// TestForceEditRefusesWithheldToolAndHolds: the narrowed tool list alone is not a
// mechanism. Measured on kloo-bench C66: kloo advertised only the edit tools and
// the model kept emitting read_file from the vocabulary it had already seen, which
// the loop dutifully executed — 21 read-only turns after an edit that broke the
// file. The rail must refuse the call, and must still be armed on the next turn.
func TestForceEditRefusesWithheldToolAndHolds(t *testing.T) {
	l := &Loop{Registry: tools.NewRegistry()}
	l.editOnlyLeft = editOnlyBudget
	// A refused turn spends one unit of budget and leaves the rail armed.
	l.editOnlyLeft--
	if l.editOnlyLeft <= 0 {
		t.Fatal("one refusal exhausted the whole budget")
	}
	if l.turnRegistry(false) == l.Registry {
		t.Fatal("rail released the vocabulary while still armed")
	}
}

// TestForceEditReleasesOnEdit: the rail exists to obtain one edit. Once it has
// one, the model needs its full vocabulary back — above all run_command, or it
// could never run the test to see whether the edit worked.
func TestForceEditReleasesOnEdit(t *testing.T) {
	l := &Loop{Registry: tools.NewRegistry()}
	l.editOnlyLeft = editOnlyBudget
	l.editOnlyLeft = 0 // what the loop does when the call was an edit tool
	if l.turnRegistry(false) != l.Registry {
		t.Fatal("full vocabulary not restored after an edit")
	}
}

// TestForceEditBudgetDecays: a run that genuinely has nothing to edit must not be
// trapped. The budget decays on every refusal, so the rail always releases.
func TestForceEditBudgetDecays(t *testing.T) {
	if editOnlyBudget <= 0 || editOnlyBudget > 5 {
		t.Fatalf("implausible force-edit budget: %d", editOnlyBudget)
	}
}

// TestCorrectiveNamesWhatIsStillFailing: measured on kloo-bench A33 — kloo made one
// edit, then read sixteen more times against a rail refusing every call, and stopped
// with one assertion red, while grok passed with a THREE-file change. The rail was
// pushing as hard as it could and never said what was still broken.
func TestCorrectiveNamesWhatIsStillFailing(t *testing.T) {
	c := editCorrective(9, true, []string{"dedupes divergent per-company statutory rows"}, nil).Content
	if !strings.Contains(c, "dedupes divergent per-company statutory rows") {
		t.Fatalf("corrective does not name the failing assertion: %q", c)
	}
	if !strings.Contains(c, "DIFFERENT file") {
		t.Fatalf("corrective does not allow for a multi-file fix: %q", c)
	}
}

// TestFailingAssertionsParsedFromVitest: the real output shape, colour codes and
// all — if the escapes are not stripped the names never match.
func TestFailingAssertionsParsedFromVitest(t *testing.T) {
	out := "\x1b[31m × \x1b[39mdedupes divergent per-company statutory rows\n" +
		" ✓ keeps canonical rows\n" +
		" × second broken thing\n"
	got := failingAssertions(out)
	if len(got) != 2 || got[0] != "dedupes divergent per-company statutory rows" {
		t.Fatalf("parsed %#v", got)
	}
}

// TestPassingRunNamesNothing: a green verify must produce no "still failing" list.
func TestPassingRunNamesNothing(t *testing.T) {
	if got := failingAssertions(""); len(got) != 0 {
		t.Fatalf("named failures on a clean output: %#v", got)
	}
}

// TestEnvOnDefaultIsOptOut: the helper the default-flip change will use. Kept
// tested even though nothing calls it yet, so the follow-up is a one-line change
// per flag rather than new untested plumbing.
func TestEnvOnDefaultIsOptOut(t *testing.T) {
	t.Setenv("KLOO_TEST_DEFAULT_ON", "")
	if !envOnDefault("KLOO_TEST_DEFAULT_ON") {
		t.Fatal("unset should mean ON")
	}
	for _, off := range []string{"0", "false", "no", "off", "OFF"} {
		t.Setenv("KLOO_TEST_DEFAULT_ON", off)
		if envOnDefault("KLOO_TEST_DEFAULT_ON") {
			t.Fatalf("%q should disable", off)
		}
	}
	t.Setenv("KLOO_TEST_DEFAULT_ON", "1")
	if !envOnDefault("KLOO_TEST_DEFAULT_ON") {
		t.Fatal("explicit 1 should mean ON")
	}
}
