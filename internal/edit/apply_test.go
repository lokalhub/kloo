package edit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyBlock(t *testing.T) {
	cases := []struct {
		name    string
		content string
		block   Block
		want    string
		wantErr error
	}{
		{
			name:    "clean apply",
			content: "alpha\nbeta\ngamma\n",
			block:   Block{Search: "beta", Replace: "BETA"},
			want:    "alpha\nBETA\ngamma\n",
		},
		{
			name:    "multi-line span replaced exactly",
			content: "one\ntwo\nthree\nfour\n",
			block:   Block{Search: "two\nthree", Replace: "2\n3"},
			want:    "one\n2\n3\nfour\n",
		},
		{
			name:    "non-matching SEARCH rejected",
			content: "alpha\nbeta\n",
			block:   Block{Search: "delta", Replace: "x"},
			wantErr: ErrSearchNotFound,
		},
		{
			name:    "whitespace near-miss rejected (no fuzzy match)",
			content: "func f() {\n\treturn 1\n}\n",                              // tab-indented
			block:   Block{Search: "func f() {\n    return 1\n}", Replace: "x"}, // spaces, not tab
			wantErr: ErrSearchNotFound,
		},
		{
			name:    "trailing-space near-miss rejected",
			content: "value = 1\n",
			block:   Block{Search: "value = 1 ", Replace: "value = 2"}, // extra trailing space
			wantErr: ErrSearchNotFound,
		},
		{
			name:    "ambiguous match rejected (fail-loud, never guess)",
			content: "x = 1\ny = 1\nz = 1\n",
			block:   Block{Search: "= 1", Replace: "= 2"}, // occurs 3x
			wantErr: ErrAmbiguousMatch,
		},
		{
			name:    "empty SEARCH is the new-file form, rejected here",
			content: "anything",
			block:   Block{Search: "", Replace: "new"},
			wantErr: ErrEmptySearch,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ApplyBlock(tc.content, tc.block)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("want %v, got err=%v", tc.wantErr, err)
				}
				if got != "" {
					t.Errorf("on error, content should be empty string, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("content mismatch\n got: %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// TestApplyToFileCleanRoundTrip: a clean apply reads, edits, and writes back.
func TestApplyToFileCleanRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	const orig = "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyToFile(path, Block{Search: "func main() {}", Replace: "func main() { return }"}); err != nil {
		t.Fatalf("ApplyToFile: %v", err)
	}
	got, _ := os.ReadFile(path)
	want := "package main\n\nfunc main() { return }\n"
	if string(got) != want {
		t.Errorf("file content\n got: %q\nwant: %q", got, want)
	}
}

// TestApplyToFileFailLoudNoWrite: a non-matching SEARCH returns ErrSearchNotFound
// and leaves the on-disk file byte-identical (no write performed).
func TestApplyToFileFailLoudNoWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.go")
	const orig = "stable contents\n"
	if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	info0, _ := os.Stat(path)

	err := ApplyToFile(path, Block{Search: "not present", Replace: "x"})
	if !errors.Is(err, ErrSearchNotFound) {
		t.Fatalf("want ErrSearchNotFound, got %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != orig {
		t.Errorf("file was modified on a failed apply: %q", got)
	}
	info1, _ := os.Stat(path)
	if info0.ModTime() != info1.ModTime() {
		t.Errorf("file mtime changed on a failed apply (a write happened)")
	}
}
