package cli

import (
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/session"
)

func ts() time.Time { return time.Date(2026, 6, 22, 13, 0, 0, 0, time.UTC) }

func saveSession(t *testing.T, st *session.Store, id, title string, updated time.Time) {
	t.Helper()
	if err := st.Save(&session.Session{ID: id, Title: title, Runs: 1, Created: updated, Updated: updated}); err != nil {
		t.Fatal(err)
	}
}

func TestChooseSessionNoneStartsFresh(t *testing.T) {
	st := session.NewStore(t.TempDir())
	sess, banner, err := chooseSession(st, config.Config{Model: "m"}, "v", "lc", SessionOpts{}, ts())
	if err != nil {
		t.Fatal(err)
	}
	if banner != "" || sess.Runs != 0 {
		t.Errorf("empty workspace should start fresh (no banner), got banner=%q runs=%d", banner, sess.Runs)
	}
	if sess.Model != "m" || sess.Verify != "v" || sess.Lint != "lc" {
		t.Errorf("fresh session didn't carry model/verify/lint: %+v", sess)
	}
}

// TestChooseSessionDefaultStartsFreshDespiteSaved is the policy change: a saved
// session in the workspace is NOT auto-resumed — a bare `kloo` always starts clean.
func TestChooseSessionDefaultStartsFreshDespiteSaved(t *testing.T) {
	st := session.NewStore(t.TempDir())
	saveSession(t, st, "20260622-120000", "rework tabs", ts())
	sess, banner, err := chooseSession(st, config.Config{}, "v", "lc", SessionOpts{}, ts())
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID == "20260622-120000" || banner != "" || sess.Runs != 0 {
		t.Errorf("default launch must start fresh, not auto-resume; got id=%q banner=%q runs=%d", sess.ID, banner, sess.Runs)
	}
}

func TestChooseSessionNewFlagForcesFresh(t *testing.T) {
	st := session.NewStore(t.TempDir())
	saveSession(t, st, "20260622-120000", "old", ts())
	sess, banner, err := chooseSession(st, config.Config{}, "v", "lc", SessionOpts{New: true}, ts())
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID == "20260622-120000" || banner != "" {
		t.Errorf("--new must start fresh even with a saved session; got id=%q banner=%q", sess.ID, banner)
	}
}

func TestChooseSessionResumeIDLoadsSpecific(t *testing.T) {
	st := session.NewStore(t.TempDir())
	saveSession(t, st, "20260622-120000", "a", ts())
	saveSession(t, st, "20260622-130000", "b", ts().Add(time.Hour))
	sess, _, err := chooseSession(st, config.Config{}, "v", "lc", SessionOpts{ResumeID: "20260622-120000"}, ts())
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "20260622-120000" {
		t.Errorf("--resume should load the named session, got %q", sess.ID)
	}
}

func TestChooseSessionResumeUnknownErrors(t *testing.T) {
	st := session.NewStore(t.TempDir())
	if _, _, err := chooseSession(st, config.Config{}, "v", "lc", SessionOpts{ResumeID: "nope"}, ts()); err == nil {
		t.Error("resuming an unknown id should error")
	}
}
