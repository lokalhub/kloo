package tools

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/lokal/kloo/internal/edit"
)

// wsAt builds a Workspace at a fresh temp root and returns it + the canonical root.
func wsAt(t *testing.T) (Workspace, string) {
	t.Helper()
	root := t.TempDir()
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return ws, canon
}

// --- task 05: read_file + list_dir ------------------------------------------

func TestReadFile(t *testing.T) {
	ws, root := wsAt(t)
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi there\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("returns exact content", func(t *testing.T) {
		got, err := ReadFile(ws, "hello.txt")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if got != "hi there\n" {
			t.Errorf("content = %q", got)
		}
	})

	t.Run("missing file is a clear os.ErrNotExist naming the path", func(t *testing.T) {
		_, err := ReadFile(ws, "nope.txt")
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("want os.ErrNotExist, got %v", err)
		}
		if err == nil || !contains(err.Error(), "nope.txt") {
			t.Errorf("error should name the path, got %v", err)
		}
	})

	t.Run("path escape is rejected", func(t *testing.T) {
		if _, err := ReadFile(ws, "../escape.txt"); !errors.Is(err, ErrPathEscape) {
			t.Errorf("want ErrPathEscape, got %v", err)
		}
	})
}

func TestListDir(t *testing.T) {
	ws, root := wsAt(t)
	mustWrite(t, filepath.Join(root, "a.go"), "x")
	mustWrite(t, filepath.Join(root, ".gitignore"), "bin/")
	mustWrite(t, filepath.Join(root, ".hidden"), "y")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Run("lists all entries with is-dir flags, filtering nothing", func(t *testing.T) {
		entries, err := ListDir(ws, ".")
		if err != nil {
			t.Fatalf("ListDir: %v", err)
		}
		got := map[string]bool{}
		for _, e := range entries {
			got[e.Name] = e.IsDir
		}
		// Dotfiles and .gitignore must still appear (no filtering at this layer).
		for _, name := range []string{"a.go", ".gitignore", ".hidden", "sub"} {
			if _, ok := got[name]; !ok {
				t.Errorf("entry %q missing from listing %v", name, names(entries))
			}
		}
		if !got["sub"] {
			t.Errorf("sub should be flagged is-dir")
		}
		if got["a.go"] {
			t.Errorf("a.go should not be flagged is-dir")
		}
	})

	t.Run("missing dir is a clear error", func(t *testing.T) {
		if _, err := ListDir(ws, "does-not-exist"); err == nil {
			t.Error("want error for missing dir")
		}
	})

	t.Run("non-directory is a clear error", func(t *testing.T) {
		if _, err := ListDir(ws, "a.go"); err == nil {
			t.Error("want error listing a non-directory")
		}
	})
}

func TestToolNameConstants(t *testing.T) {
	cases := map[string]string{
		NameReadFile:  "read_file",
		NameListDir:   "list_dir",
		NameWriteFile: "write_file",
		NameEditFile:  "edit_file",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("tool name constant = %q, want %q", got, want)
		}
	}
}

// --- task 06: write_file + edit_file ----------------------------------------

func TestWriteFile(t *testing.T) {
	ws, root := wsAt(t)

	t.Run("creates a new file with exact content", func(t *testing.T) {
		if err := WriteFile(ws, "out/new.txt", "fresh\n"); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, _ := os.ReadFile(filepath.Join(root, "out", "new.txt"))
		if string(got) != "fresh\n" {
			t.Errorf("content = %q", got)
		}
	})

	t.Run("overwrites an existing file (write_file may clobber)", func(t *testing.T) {
		p := filepath.Join(root, "over.txt")
		mustWrite(t, p, "OLD")
		if err := WriteFile(ws, "over.txt", "NEW"); err != nil {
			t.Fatalf("WriteFile overwrite: %v", err)
		}
		got, _ := os.ReadFile(p)
		if string(got) != "NEW" {
			t.Errorf("content = %q, want NEW (overwrite)", got)
		}
	})

	t.Run("path escape rejected", func(t *testing.T) {
		if err := WriteFile(ws, "../evil.txt", "x"); !errors.Is(err, ErrPathEscape) {
			t.Errorf("want ErrPathEscape, got %v", err)
		}
	})
}

func TestEditFile(t *testing.T) {
	ws, root := wsAt(t)

	block := func(file, search, replace string) string {
		s := "<<<<<<< SEARCH\n" + search + "\n=======\n" + replace + "\n>>>>>>> REPLACE"
		if search == "" {
			s = "<<<<<<< SEARCH\n=======\n" + replace + "\n>>>>>>> REPLACE"
		}
		return file + "\n```\n" + s + "\n```\n"
	}

	t.Run("clean apply edits the file exactly", func(t *testing.T) {
		p := filepath.Join(root, "edit.go")
		mustWrite(t, p, "package main\n\nfunc main() {}\n")
		if err := EditFile(ws, "edit.go", block("edit.go", "func main() {}", "func main() { return }")); err != nil {
			t.Fatalf("EditFile: %v", err)
		}
		got, _ := os.ReadFile(p)
		if string(got) != "package main\n\nfunc main() { return }\n" {
			t.Errorf("content = %q", got)
		}
	})

	t.Run("surfaces ErrSearchNotFound, file unchanged", func(t *testing.T) {
		p := filepath.Join(root, "nf.go")
		const orig = "untouched\n"
		mustWrite(t, p, orig)
		err := EditFile(ws, "nf.go", block("nf.go", "ABSENT", "x"))
		if !errors.Is(err, edit.ErrSearchNotFound) {
			t.Fatalf("want edit.ErrSearchNotFound, got %v", err)
		}
		got, _ := os.ReadFile(p)
		if string(got) != orig {
			t.Errorf("file changed on failed edit: %q", got)
		}
	})

	t.Run("surfaces ErrMalformedBlock, nothing written", func(t *testing.T) {
		p := filepath.Join(root, "mal.go")
		const orig = "keep\n"
		mustWrite(t, p, orig)
		// malformed: missing the ======= divider
		bad := "mal.go\n```\n<<<<<<< SEARCH\nkeep\nx\n>>>>>>> REPLACE\n```\n"
		err := EditFile(ws, "mal.go", bad)
		if !errors.Is(err, edit.ErrMalformedBlock) {
			t.Fatalf("want edit.ErrMalformedBlock, got %v", err)
		}
		got, _ := os.ReadFile(p)
		if string(got) != orig {
			t.Errorf("file changed on malformed block: %q", got)
		}
	})

	t.Run("new-file via empty SEARCH creates the file", func(t *testing.T) {
		// Body is line-anchored: the engine supplies the trailing newline.
		if err := EditFile(ws, "made.go", block("made.go", "", "package made")); err != nil {
			t.Fatalf("EditFile new-file: %v", err)
		}
		got, _ := os.ReadFile(filepath.Join(root, "made.go"))
		if string(got) != "package made\n" {
			t.Errorf("content = %q", got)
		}
	})

	t.Run("path escape rejected", func(t *testing.T) {
		if err := EditFile(ws, "../x.go", block("x.go", "a", "b")); !errors.Is(err, ErrPathEscape) {
			t.Errorf("want ErrPathEscape, got %v", err)
		}
	})
}

// --- helpers ----------------------------------------------------------------

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func names(entries []DirEntry) []string {
	var out []string
	for _, e := range entries {
		out = append(out, e.Name)
	}
	sort.Strings(out)
	return out
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
