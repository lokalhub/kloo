package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEditFileAppendForm is the end-to-end half of the append fix: a model that
// wants to ADD a new function to an existing file sends a block with an EMPTY
// SEARCH section and edit_file lands it at the end of the file.
func TestEditFileAppendForm(t *testing.T) {
	ctx := context.Background()
	ws, dir := wsAt(t)
	p := filepath.Join(dir, "lib.rs")
	if err := os.WriteFile(p, []byte("pub fn a() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	diffs := []struct {
		name string
		diff string
	}{
		{"bare", "<<<<<<< SEARCH\n=======\npub fn b() {}\n>>>>>>> REPLACE"},
		{"fenced", "```rust\n<<<<<<< SEARCH\n=======\npub fn b() {}\n>>>>>>> REPLACE\n```"},
	}
	for _, d := range diffs {
		t.Run(d.name, func(t *testing.T) {
			if err := os.WriteFile(p, []byte("pub fn a() {}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := DefaultRegistry(ws).Dispatch(ctx, Call{Name: "edit_file", Args: map[string]any{"path": "lib.rs", "diff": d.diff}}); err != nil {
				t.Fatalf("append through edit_file must succeed, got %v", err)
			}
			got, _ := os.ReadFile(p)
			if string(got) != "pub fn a() {}\npub fn b() {}\n" {
				t.Errorf("content = %q", got)
			}
		})
	}
}

// TestEditFileNoBlockErrorIsActionable is the real lesson of the measured run: the
// rejection a model gets back must make a RETRY DIFFER from the attempt. The old
// message repeated the same generic marker list three times and the model re-sent
// the same shape until the churn rail stopped the run with zero files changed.
//
// So the error must name, for THIS file: that no markers were found at all, the
// empty-SEARCH shape for adding code, and a real anchor line copied out of the
// file for an in-place change.
func TestEditFileNoBlockErrorIsActionable(t *testing.T) {
	ctx := context.Background()
	ws, dir := wsAt(t)
	p := filepath.Join(dir, "lib.rs")
	const initial = "pub fn a() {}\n\nimpl Thing {\n    pub fn go(&self) {}\n}\n"
	if err := os.WriteFile(p, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	// The measured shape: the model sent just the new code, no markers.
	_, err := DefaultRegistry(ws).Dispatch(ctx, Call{Name: "edit_file", Args: map[string]any{
		"path": "lib.rs",
		"diff": "pub fn b() -> u32 {\n    7\n}\n",
	}})
	if err == nil {
		t.Fatal("a diff with no markers must still ERROR")
	}
	msg := err.Error()

	for _, want := range []string{
		"no SEARCH/REPLACE markers",      // names the actual shape that arrived
		"lib.rs",                         // names the file
		"leave the SEARCH section EMPTY", // the append shape, by name
		"<<<<<<< SEARCH\n=======\n",      // the append shape, literally copyable
		"}",                              // a real anchor line out of the file
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error must contain %q so a retry can differ.\ngot: %s", want, msg)
		}
	}

	got, _ := os.ReadFile(p)
	if string(got) != initial {
		t.Errorf("file must be untouched, got %q", got)
	}
}

// TestEditFileDescriptionDocumentsAppend: a shape the model is never told about is
// a shape it will never send. The append form must be in the tool's own schema.
func TestEditFileDescriptionDocumentsAppend(t *testing.T) {
	ws, _ := wsAt(t)
	tool, ok := DefaultRegistry(ws).Lookup(NameEditFile)
	if !ok {
		t.Fatal("edit_file not registered")
	}
	desc := tool.Schema().Properties["diff"].Description + " " + tool.Description()
	for _, want := range []string{"end of the file", "SEARCH section empty"} {
		if !strings.Contains(desc, want) {
			t.Errorf("edit_file must document the append form (%q missing).\ngot: %s", want, desc)
		}
	}
}
