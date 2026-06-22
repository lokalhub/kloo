package edit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestApplyMultiBlockSingleFile: two blocks targeting one file both apply.
func TestApplyMultiBlockSingleFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.go")
	write(t, f, "one\ntwo\nthree\n")

	res, err := Apply([]Block{
		{File: f, Search: "one", Replace: "1"},
		{File: f, Search: "three", Replace: "3"},
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got := read(t, f); got != "1\ntwo\n3\n" {
		t.Errorf("content = %q", got)
	}
	if len(res.Files) != 1 || !res.Files[0].Written {
		t.Errorf("result = %+v", res.Files)
	}
}

// TestApplyMultiFile: blocks across two files both written.
func TestApplyMultiFile(t *testing.T) {
	dir := t.TempDir()
	fa := filepath.Join(dir, "a.go")
	fb := filepath.Join(dir, "b.go")
	write(t, fa, "aaa\n")
	write(t, fb, "bbb\n")

	if _, err := Apply([]Block{
		{File: fa, Search: "aaa", Replace: "AAA"},
		{File: fb, Search: "bbb", Replace: "BBB"},
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if read(t, fa) != "AAA\n" || read(t, fb) != "BBB\n" {
		t.Errorf("a=%q b=%q", read(t, fa), read(t, fb))
	}
}

// TestApplyOneFailingBlockAbortsItsFileOnly: a bad block aborts only its file;
// other files still apply, and the failed file is byte-identical.
func TestApplyOneFailingBlockAbortsItsFileOnly(t *testing.T) {
	dir := t.TempDir()
	fa := filepath.Join(dir, "a.go")
	fb := filepath.Join(dir, "b.go")
	const aOrig = "good\nstuff\n"
	write(t, fa, aOrig)
	write(t, fb, "keep\n")

	res, err := Apply([]Block{
		{File: fa, Search: "good", Replace: "GOOD"}, // ok
		{File: fa, Search: "ABSENT", Replace: "x"},  // fails → aborts file A
		{File: fb, Search: "keep", Replace: "KEEP"}, // ok → file B written
	})
	if err == nil {
		t.Fatal("want non-nil error (file A failed)")
	}
	if !errors.Is(err, ErrSearchNotFound) {
		t.Errorf("joined error should carry ErrSearchNotFound, got %v", err)
	}
	// File A untouched (no partial write).
	if got := read(t, fa); got != aOrig {
		t.Errorf("file A got a partial write: %q", got)
	}
	// File B applied.
	if got := read(t, fb); got != "KEEP\n" {
		t.Errorf("file B not applied: %q", got)
	}
	// Result reports A failed, B written.
	byPath := map[string]FileResult{}
	for _, fr := range res.Files {
		byPath[fr.Path] = fr
	}
	if byPath[fa].Written || byPath[fa].Err == nil {
		t.Errorf("A result = %+v, want failed+unwritten", byPath[fa])
	}
	if !byPath[fb].Written {
		t.Errorf("B result = %+v, want written", byPath[fb])
	}
}

// TestApplyNewFilePlusEdit: an empty-SEARCH create block + an edit on an existing
// file in one batch — both happen.
func TestApplyNewFilePlusEdit(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "exists.go")
	created := filepath.Join(dir, "sub", "new.go")
	write(t, existing, "hello\n")

	if _, err := Apply([]Block{
		{File: created, Search: "", Replace: "package new\n"}, // create (+ parent dir)
		{File: existing, Search: "hello", Replace: "HELLO"},   // edit
	}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if read(t, created) != "package new\n" {
		t.Errorf("created content = %q", read(t, created))
	}
	if read(t, existing) != "HELLO\n" {
		t.Errorf("edited content = %q", read(t, existing))
	}
}

// TestApplyDeterminism: applying the same batch twice (fresh fixtures) yields
// identical file contents and an equivalent Result ordering.
func TestApplyDeterminism(t *testing.T) {
	run := func() (string, string, []string) {
		dir := t.TempDir()
		fa := filepath.Join(dir, "z.go")
		fb := filepath.Join(dir, "a.go")
		write(t, fa, "z\n")
		write(t, fb, "a\n")
		res, _ := Apply([]Block{
			{File: fa, Search: "z", Replace: "Z"},
			{File: fb, Search: "a", Replace: "A"},
		})
		var order []string
		for _, fr := range res.Files {
			order = append(order, filepath.Base(fr.Path))
		}
		return read(t, fa), read(t, fb), order
	}
	a1, b1, o1 := run()
	a2, b2, o2 := run()
	if a1 != a2 || b1 != b2 {
		t.Errorf("content not deterministic: %q/%q vs %q/%q", a1, b1, a2, b2)
	}
	// Sorted-by-path order: a.go before z.go, both runs.
	if len(o1) != 2 || o1[0] != "a.go" || o1[1] != "z.go" || o1[0] != o2[0] || o1[1] != o2[1] {
		t.Errorf("result order not deterministic/sorted: %v vs %v", o1, o2)
	}
}

// TestApplyNewFileRefusesClobber: an empty-SEARCH create on an existing file
// fails with ErrFileExists and does not overwrite it.
func TestApplyNewFileRefusesClobber(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "there.go")
	write(t, f, "ORIGINAL\n")

	res, err := Apply([]Block{{File: f, Search: "", Replace: "NEW"}})
	if !errors.Is(err, ErrFileExists) {
		t.Fatalf("want ErrFileExists, got %v", err)
	}
	if read(t, f) != "ORIGINAL\n" {
		t.Errorf("file was clobbered: %q", read(t, f))
	}
	if res.Files[0].Written {
		t.Errorf("result should report unwritten")
	}
}
