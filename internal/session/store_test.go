package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/llm"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	ws := t.TempDir()
	st := NewStore(ws)
	now := time.Date(2026, 6, 22, 13, 0, 0, 0, time.UTC)
	sess := &Session{
		ID: NewID(now), Title: Title("rework the tabs"), Model: "snappy", Verify: "npm run build",
		Runs: 1, Created: now, Updated: now,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "rework the tabs"}, {Role: llm.RoleAssistant, Content: "done"}},
	}
	if err := st.Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := st.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Title != "rework the tabs" || got.Model != "snappy" || got.Runs != 1 || len(got.Messages) != 2 {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

func TestSaveWritesSelfIgnoringGitignore(t *testing.T) {
	ws := t.TempDir()
	st := NewStore(ws)
	now := time.Date(2026, 6, 22, 13, 0, 0, 0, time.UTC)
	if err := st.Save(&Session{ID: NewID(now), Created: now, Updated: now}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(ws, ".kloo", ".gitignore"))
	if err != nil {
		t.Fatalf(".kloo/.gitignore not written: %v", err)
	}
	if string(data) != "*\n" {
		t.Errorf(".gitignore = %q, want %q (self-ignore so transcripts aren't committed)", string(data), "*\n")
	}
}

func TestListNewestFirstSkipsCorrupt(t *testing.T) {
	ws := t.TempDir()
	st := NewStore(ws)
	base := time.Date(2026, 6, 22, 13, 0, 0, 0, time.UTC)
	older := &Session{ID: "20260622-130000", Title: "older", Created: base, Updated: base}
	newer := &Session{ID: "20260622-140000", Title: "newer", Created: base, Updated: base.Add(time.Hour)}
	if err := st.Save(older); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(newer); err != nil {
		t.Fatal(err)
	}
	// A corrupt file must be skipped, not break listing.
	if err := os.WriteFile(filepath.Join(ws, ".kloo", "sessions", "bad.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	metas, err := st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("got %d metas, want 2 (corrupt skipped)", len(metas))
	}
	if metas[0].Title != "newer" || metas[1].Title != "older" {
		t.Errorf("not sorted newest-first: %q, %q", metas[0].Title, metas[1].Title)
	}
}

func TestListNoStoreIsEmpty(t *testing.T) {
	metas, err := NewStore(t.TempDir()).List()
	if err != nil {
		t.Fatalf("List on empty workspace: %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("want no sessions, got %d", len(metas))
	}
}

func TestTitleBounded(t *testing.T) {
	long := Title(string(make([]byte, 0)) + "x" + string(rune('a')))
	_ = long
	if got := Title("a\nb"); got != "a b" {
		t.Errorf("Title newlines = %q, want %q", got, "a b")
	}
	if got := Title("short"); got != "short" {
		t.Errorf("Title short = %q", got)
	}
	big := ""
	for i := 0; i < 100; i++ {
		big += "x"
	}
	if got := Title(big); len([]rune(got)) != 58 { // 57 + ellipsis
		t.Errorf("Title long len = %d, want 58", len([]rune(got)))
	}
}
