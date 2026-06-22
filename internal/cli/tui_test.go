package cli

import (
	"bytes"
	"strings"
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
	sess, banner, err := chooseSession(st, config.Config{Model: "m"}, "v", SessionOpts{}, strings.NewReader(""), &bytes.Buffer{}, ts())
	if err != nil {
		t.Fatal(err)
	}
	if banner != "" || sess.Runs != 0 {
		t.Errorf("empty workspace should start fresh (no banner), got banner=%q runs=%d", banner, sess.Runs)
	}
	if sess.Model != "m" || sess.Verify != "v" {
		t.Errorf("fresh session didn't carry model/verify: %+v", sess)
	}
}

func TestChooseSessionSingleAutoResumes(t *testing.T) {
	st := session.NewStore(t.TempDir())
	saveSession(t, st, "20260622-120000", "rework tabs", ts())
	sess, banner, err := chooseSession(st, config.Config{}, "v", SessionOpts{}, strings.NewReader(""), &bytes.Buffer{}, ts())
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "20260622-120000" {
		t.Errorf("single session should auto-resume, got id %q", sess.ID)
	}
	if !strings.Contains(banner, "resumed session") || !strings.Contains(banner, "rework tabs") {
		t.Errorf("resume banner missing: %q", banner)
	}
}

func TestChooseSessionNewFlagForcesFresh(t *testing.T) {
	st := session.NewStore(t.TempDir())
	saveSession(t, st, "20260622-120000", "old", ts())
	sess, banner, err := chooseSession(st, config.Config{}, "v", SessionOpts{New: true}, strings.NewReader(""), &bytes.Buffer{}, ts())
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
	sess, _, err := chooseSession(st, config.Config{}, "v", SessionOpts{ResumeID: "20260622-120000"}, strings.NewReader(""), &bytes.Buffer{}, ts())
	if err != nil {
		t.Fatal(err)
	}
	if sess.ID != "20260622-120000" {
		t.Errorf("--resume should load the named session, got %q", sess.ID)
	}
}

func TestChooseSessionResumeUnknownErrors(t *testing.T) {
	st := session.NewStore(t.TempDir())
	if _, _, err := chooseSession(st, config.Config{}, "v", SessionOpts{ResumeID: "nope"}, strings.NewReader(""), &bytes.Buffer{}, ts()); err == nil {
		t.Error("resuming an unknown id should error")
	}
}

func TestChooseSessionMultiplePromptsAndPicks(t *testing.T) {
	st := session.NewStore(t.TempDir())
	saveSession(t, st, "20260622-120000", "older", ts())
	saveSession(t, st, "20260622-130000", "newer", ts().Add(time.Hour))
	out := &bytes.Buffer{}
	// "2" picks the second listed (newest is listed first, so #2 = "older").
	sess, banner, err := chooseSession(st, config.Config{}, "v", SessionOpts{}, strings.NewReader("2\n"), out, ts())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Multiple kloo sessions") {
		t.Errorf("expected a picker prompt:\n%s", out.String())
	}
	if sess.Title != "older" || !strings.Contains(banner, "older") {
		t.Errorf("picked the wrong session: title=%q banner=%q", sess.Title, banner)
	}
}

func TestChooseSessionMultipleNewChoice(t *testing.T) {
	st := session.NewStore(t.TempDir())
	saveSession(t, st, "20260622-120000", "a", ts())
	saveSession(t, st, "20260622-130000", "b", ts().Add(time.Hour))
	sess, banner, err := chooseSession(st, config.Config{}, "v", SessionOpts{}, strings.NewReader("n\n"), &bytes.Buffer{}, ts())
	if err != nil {
		t.Fatal(err)
	}
	if banner != "" || sess.Runs != 0 {
		t.Errorf("choosing 'n' should start fresh; got banner=%q", banner)
	}
}
