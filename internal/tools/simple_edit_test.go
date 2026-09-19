package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestSimpleEditSwapsTheEditTool: with KLOO_SIMPLE_EDIT the vocabulary offers
// grok's three-field edit tool INSTEAD of the fenced SEARCH/REPLACE one. Instead
// of, not alongside — two edit tools would confound the measurement, since the
// question is whether the FORMAT suppresses editing.
func TestSimpleEditSwapsTheEditTool(t *testing.T) {
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("KLOO_SIMPLE_EDIT", "")
	names := map[string]bool{}
	for _, tl := range DefaultRegistry(ws).Tools() {
		names[tl.Name()] = true
	}
	if !names[NameEditFile] || names["search_replace"] {
		t.Errorf("default vocabulary changed: edit_file=%v search_replace=%v", names[NameEditFile], names["search_replace"])
	}
	t.Setenv("KLOO_SIMPLE_EDIT", "1")
	names = map[string]bool{}
	for _, tl := range DefaultRegistry(ws).Tools() {
		names[tl.Name()] = true
	}
	if names[NameEditFile] || !names["search_replace"] {
		t.Errorf("with the flag: edit_file=%v search_replace=%v, want the swap", names[NameEditFile], names["search_replace"])
	}
}

// TestSearchReplaceEditsThroughTheSameEngine: the new tool must go through the
// SEARCH/REPLACE engine, so scope policy, clobber guards and edit accounting all
// behave exactly as they do for edit_file. Only the model-facing shape differs.
func TestSearchReplaceEditsThroughTheSameEngine(t *testing.T) {
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(ws.Root(), "a.txt")
	if err := os.WriteFile(p, []byte("alpha\nbeta\ngamma\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := searchReplaceTool{ws}
	_, err = tool.Invoke(context.Background(), Call{Args: map[string]any{
		"file_path": "a.txt", "old_string": "beta", "new_string": "BETA",
	}})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "alpha\nBETA\ngamma\n" {
		t.Errorf("file = %q, want beta replaced", string(got))
	}
}

// TestSearchReplaceRejectsAMissingMatch: a silent no-op is the failure mode that
// cost kloo a whole class of edits before (edit_file once reported success while
// writing nothing). The simpler shape must not reintroduce it.
func TestSearchReplaceRejectsAMissingMatch(t *testing.T) {
	ws, werr := NewWorkspace(t.TempDir())
	if werr != nil {
		t.Fatal(werr)
	}
	if err := os.WriteFile(filepath.Join(ws.Root(), "b.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := searchReplaceTool{ws}.Invoke(context.Background(), Call{Args: map[string]any{
		"file_path": "b.txt", "old_string": "not-present", "new_string": "x",
	}})
	if err == nil {
		t.Error("a non-matching old_string reported success instead of erroring")
	}
}
