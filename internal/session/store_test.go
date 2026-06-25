package session

import (
	"os"
	"path/filepath"
	"strings"
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

// TestSaveLoadTranscript: the human-readable display log survives the round-trip
// (so a resumed session can replay prior turns), and is omitted from JSON when empty.
func TestSaveLoadTranscript(t *testing.T) {
	st := NewStore(t.TempDir())
	now := time.Date(2026, 6, 23, 17, 0, 0, 0, time.UTC)
	sess := &Session{
		ID: NewID(now), Created: now, Updated: now,
		Transcript: []DisplayItem{
			{Kind: "user", Text: "build the app"},
			{Kind: "assistant", Text: "scaffolding now"},
			{Kind: "tool", Text: "ran: npm run build [exit 0]"},
		},
	}
	if err := st.Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := st.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got.Transcript) != 3 || got.Transcript[0].Kind != "user" || got.Transcript[2].Text != "ran: npm run build [exit 0]" {
		t.Errorf("transcript round-trip mismatch: %+v", got.Transcript)
	}

	// Empty transcript ⇒ omitted from the JSON (omitempty).
	empty := &Session{ID: "empty", Created: now, Updated: now}
	if err := st.Save(empty); err != nil {
		t.Fatalf("Save empty: %v", err)
	}
	ed, _ := os.ReadFile(filepath.Join(st.dir, "empty.json"))
	if strings.Contains(string(ed), "transcript") {
		t.Errorf("empty transcript should be omitted from JSON:\n%s", ed)
	}
}

// TestSaveLoadLint: the resolved fast-advisory-lint command survives the round-trip
// (resume parity), is omitted from JSON when empty, and a pre-existing session JSON
// without a "lint" key still decodes (back-compat).
func TestSaveLoadLint(t *testing.T) {
	st := NewStore(t.TempDir())
	now := time.Date(2026, 6, 24, 10, 0, 0, 0, time.UTC)

	sess := &Session{ID: NewID(now), Verify: "go test ./...", Lint: "gofmt -l", Created: now, Updated: now}
	if err := st.Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := st.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Lint != "gofmt -l" {
		t.Errorf("lint did not round-trip, got %q", got.Lint)
	}

	// Empty lint ⇒ omitted from JSON (omitempty).
	empty := &Session{ID: "blank", Created: now, Updated: now}
	if err := st.Save(empty); err != nil {
		t.Fatalf("Save empty: %v", err)
	}
	ed, _ := os.ReadFile(filepath.Join(st.dir, "blank.json"))
	if strings.Contains(string(ed), `"lint"`) {
		t.Errorf("empty lint should be omitted from JSON:\n%s", ed)
	}

	// Back-compat: a session JSON written before the lint field still loads (Lint="").
	legacy := `{"id":"legacy","title":"old","model":"m","verify":"go test ./...","runs":1,"created":"2026-06-01T00:00:00Z","updated":"2026-06-01T00:00:00Z"}`
	if err := os.WriteFile(filepath.Join(st.dir, "legacy.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	old, err := st.Load("legacy")
	if err != nil {
		t.Fatalf("Load legacy: %v", err)
	}
	if old.Lint != "" || old.Verify != "go test ./..." {
		t.Errorf("legacy session without a lint key should load unchanged, got %+v", old)
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
