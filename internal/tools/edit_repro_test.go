package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestEditFileFencedAndUnfenced locks in the fix for the silent-no-op bug: the
// edit_file tool must apply a SEARCH/REPLACE block whether or not the model wrapped
// it in a ``` fence, and must ERROR (never report false success) when the diff
// carries no block at all — otherwise a small model loops re-"editing" a file that
// never changed.
func TestEditFileFencedAndUnfenced(t *testing.T) {
	ctx := context.Background()
	edit := func(t *testing.T, diff string) (Result, error, string) {
		t.Helper()
		ws, dir := wsAt(t)
		p := filepath.Join(dir, "a.txt")
		if err := os.WriteFile(p, []byte("hello world\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		res, err := DefaultRegistry(ws).Dispatch(ctx, Call{Name: "edit_file", Args: map[string]any{"path": "a.txt", "diff": diff}})
		got, _ := os.ReadFile(p)
		return res, err, string(got)
	}

	t.Run("fenced applies", func(t *testing.T) {
		_, err, got := edit(t, "```\n<<<<<<< SEARCH\nhello world\n=======\ngoodbye world\n>>>>>>> REPLACE\n```")
		if err != nil || got != "goodbye world\n" {
			t.Errorf("fenced edit failed: err=%v file=%q", err, got)
		}
	})

	t.Run("unfenced applies", func(t *testing.T) {
		_, err, got := edit(t, "<<<<<<< SEARCH\nhello world\n=======\ngoodbye world\n>>>>>>> REPLACE")
		if err != nil || got != "goodbye world\n" {
			t.Errorf("unfenced (bare-marker) edit must apply now: err=%v file=%q", err, got)
		}
	})

	t.Run("no block errors, file untouched", func(t *testing.T) {
		_, err, got := edit(t, "please change hello to goodbye")
		if err == nil {
			t.Errorf("a diff with no SEARCH/REPLACE block must ERROR, not report success")
		}
		if got != "hello world\n" {
			t.Errorf("file must be left untouched on a non-edit, got %q", got)
		}
	})
}
