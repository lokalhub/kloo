package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEditFileAppliesEveryBlockInOneCall pins the capability the tool now advertises.
//
// The engine has always applied a list of blocks (internal/edit.Apply), but the tool's
// own description said "a SEARCH/REPLACE block", singular — so a model changing five
// lines in one file spent five turns on it, and with one tool call per turn that is five
// model round trips. Measured on a real task (a stylesheet edit against a slow endpoint)
// that was minutes of wall clock for twenty lines of CSS. The wording now tells the model
// to batch; this test makes sure the behaviour behind the wording is real.
func TestEditFileAppliesEveryBlockInOneCall(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "styles.css")
	const before = "a { color: red; }\nb { color: green; }\nc { color: blue; }\n"
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	diff := strings.Join([]string{
		"<<<<<<< SEARCH", "a { color: red; }", "=======", "a { color: black; }", ">>>>>>> REPLACE",
		"<<<<<<< SEARCH", "c { color: blue; }", "=======", "c { color: white; }", ">>>>>>> REPLACE",
	}, "\n")

	ws, err := NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := DefaultRegistry(ws).Dispatch(context.Background(), Call{
		Name: NameEditFile,
		Args: map[string]any{"path": "styles.css", "diff": diff},
	})
	if err != nil {
		t.Fatalf("edit_file with two blocks failed: %v", err)
	}
	if !strings.Contains(res.Output, "edited") {
		t.Errorf("unexpected result: %q", res.Output)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "a { color: black; }\nb { color: green; }\nc { color: white; }\n"
	if string(got) != want {
		t.Errorf("both blocks should have applied in one call:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// TestEditToolAdvertisesBatching: the capability is worthless if the model is not told.
// The description and the diff parameter must both say that several blocks are allowed
// in one call — that sentence is the entire fix for the round-trip cost.
func TestEditToolAdvertisesBatching(t *testing.T) {
	ws, err := NewWorkspace(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tool := editFileTool{ws}
	if !strings.Contains(tool.Description(), "one or more") {
		t.Errorf("description should offer multiple blocks: %q", tool.Description())
	}
	diffDesc := tool.Schema().Properties["diff"].Description
	for _, want := range []string{"One or more", "Several blocks"} {
		if !strings.Contains(diffDesc, want) {
			t.Errorf("diff schema missing %q:\n%s", want, diffDesc)
		}
	}
}
