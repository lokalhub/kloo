package edit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateFile(t *testing.T) {
	t.Run("creates new file with exact content", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "new.go")
		if err := CreateFile(path, Block{Search: "", Replace: "package main\n"}); err != nil {
			t.Fatalf("CreateFile: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "package main\n" {
			t.Errorf("content = %q", got)
		}
	})

	t.Run("creates missing parent dirs", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "a", "b", "c", "deep.go")
		if err := CreateFile(path, Block{Replace: "x"}); err != nil {
			t.Fatalf("CreateFile: %v", err)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("file not created: %v", err)
		}
	})

	t.Run("refuses to clobber existing file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "exists.go")
		const orig = "ORIGINAL\n"
		if err := os.WriteFile(path, []byte(orig), 0o644); err != nil {
			t.Fatal(err)
		}
		err := CreateFile(path, Block{Replace: "CLOBBERED"})
		if !errors.Is(err, ErrFileExists) {
			t.Fatalf("want ErrFileExists, got %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != orig {
			t.Errorf("existing file was clobbered: %q", got)
		}
	})

	t.Run("empty SEARCH and empty REPLACE creates empty file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "empty.txt")
		if err := CreateFile(path, Block{Search: "", Replace: ""}); err != nil {
			t.Fatalf("CreateFile: %v", err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("file not created: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("want empty file, got %q", got)
		}
	})

	t.Run("file mode is 0644", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "perm.go")
		if err := CreateFile(path, Block{Replace: "x"}); err != nil {
			t.Fatal(err)
		}
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0o644 {
			t.Errorf("perm = %o, want 0644", info.Mode().Perm())
		}
	})
}

// TestApplyToFileNewFormDelegates: ApplyToFile with an empty SEARCH routes to the
// new-file create path when the file is absent, and to the append path when it is
// already there.
func TestApplyToFileNewFormDelegates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "made.go")
	if err := ApplyToFile(path, Block{Search: "", Replace: "created via ApplyToFile\n"}); err != nil {
		t.Fatalf("ApplyToFile (new form): %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "created via ApplyToFile\n" {
		t.Errorf("content = %q", got)
	}
	// A second empty-SEARCH call APPENDS rather than clobbering: the form means
	// "add at the end" and routes to CreateFile only when the file is absent.
	if err := ApplyToFile(path, Block{Search: "", Replace: "again\n"}); err != nil {
		t.Errorf("second empty-SEARCH call must append, got %v", err)
	}
	got, _ = os.ReadFile(path)
	if string(got) != "created via ApplyToFile\nagain\n" {
		t.Errorf("content after append = %q", got)
	}
}
