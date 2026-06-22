package edit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestApplySearchReplace is the consolidated, table-driven DoD suite for the
// edit engine (conventions/testing.md §Pattern 1). Each row drives the REAL
// engine path end-to-end — Parse → Apply — against files in t.TempDir(), and
// asserts the final on-disk content byte-for-byte (or the absence of any write).
//
// The four design-doc §5 DoD rows are named exactly: "clean apply",
// "non-matching SEARCH rejected", "multi-block", "new file (empty SEARCH)".
// A "whitespace near-miss rejected" row makes the no-fuzzy-match rule explicit
// at the integration level.
func TestApplySearchReplace(t *testing.T) {
	cases := []struct {
		name string
		// initial files to create before applying (relative name -> content);
		// names not listed do not exist yet.
		files map[string]string
		// the model's SEARCH/REPLACE output, referencing the relative names.
		message string
		// sentinel the joined error must match (errors.Is), or nil for success.
		wantErr error
		// expected final content per file (relative name -> content). For the
		// fail-loud rows this is the ORIGINAL content (proving no write).
		wantContent map[string]string
		// files that must not exist after applying.
		wantAbsent []string
	}{
		{
			name:        "clean apply",
			files:       map[string]string{"a.go": "alpha\nbeta\ngamma\n"},
			message:     "a.go\n```go\n<<<<<<< SEARCH\nbeta\n=======\nBETA\n>>>>>>> REPLACE\n```\n",
			wantContent: map[string]string{"a.go": "alpha\nBETA\ngamma\n"},
		},
		{
			name:        "non-matching SEARCH rejected",
			files:       map[string]string{"a.go": "stable contents\n"},
			message:     "a.go\n```\n<<<<<<< SEARCH\nNOT PRESENT\n=======\nx\n>>>>>>> REPLACE\n```\n",
			wantErr:     ErrSearchNotFound,
			wantContent: map[string]string{"a.go": "stable contents\n"}, // byte-identical: no write
		},
		{
			name:  "whitespace near-miss rejected",
			files: map[string]string{"a.go": "func f() {\n\treturn 1\n}\n"}, // tab-indented
			// SEARCH uses 4 spaces, not a tab → must NOT fuzzy-match.
			message:     "a.go\n```\n<<<<<<< SEARCH\nfunc f() {\n    return 1\n}\n=======\nx\n>>>>>>> REPLACE\n```\n",
			wantErr:     ErrSearchNotFound,
			wantContent: map[string]string{"a.go": "func f() {\n\treturn 1\n}\n"},
		},
		{
			name: "multi-block",
			files: map[string]string{
				"a.go": "one\ntwo\n",
				"b.go": "keep me\n",
			},
			// a.go: two good blocks (both apply). b.go: one good + one bad
			// (whole file aborts → byte-identical). Per-file atomicity.
			message: "a.go\n```\n<<<<<<< SEARCH\none\n=======\n1\n>>>>>>> REPLACE\n```\n" +
				"a.go\n```\n<<<<<<< SEARCH\ntwo\n=======\n2\n>>>>>>> REPLACE\n```\n" +
				"b.go\n```\n<<<<<<< SEARCH\nkeep me\n=======\nKEPT\n>>>>>>> REPLACE\n```\n" +
				"b.go\n```\n<<<<<<< SEARCH\nNOPE\n=======\nx\n>>>>>>> REPLACE\n```\n",
			wantErr: ErrSearchNotFound,
			wantContent: map[string]string{
				"a.go": "1\n2\n",    // both blocks applied
				"b.go": "keep me\n", // failing block aborted the file: no partial write
			},
		},
		{
			name:        "new file (empty SEARCH)",
			files:       map[string]string{}, // new.go does not exist yet
			message:     "pkg/new.go\n```go\n<<<<<<< SEARCH\n=======\npackage pkg\n>>>>>>> REPLACE\n```\n",
			wantContent: map[string]string{"pkg/new.go": "package pkg\n"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tc.files {
				p := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			blocks, err := Parse(tc.message)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			// Retarget each block's relative filename to this run's temp dir.
			for i := range blocks {
				blocks[i].File = filepath.Join(dir, blocks[i].File)
			}

			_, applyErr := Apply(blocks)
			if tc.wantErr != nil {
				if !errors.Is(applyErr, tc.wantErr) {
					t.Fatalf("want errors.Is(%v), got %v", tc.wantErr, applyErr)
				}
			} else if applyErr != nil {
				t.Fatalf("unexpected Apply error: %v", applyErr)
			}

			for name, want := range tc.wantContent {
				got, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					t.Fatalf("read %s: %v", name, err)
				}
				if string(got) != want {
					t.Errorf("%s content\n got: %q\nwant: %q", name, got, want)
				}
			}
			for _, name := range tc.wantAbsent {
				if _, err := os.Stat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("%s should not exist", name)
				}
			}
		})
	}
}
