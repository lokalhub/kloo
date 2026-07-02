package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

// initGitRepo makes a temp git repo with an initial commit and returns its root.
func initGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-m", "init")
	return root
}

// TestChangedFilesSortedTrackedAndUntracked: modified tracked files and new untracked
// files are both reported, sorted and de-duplicated.
func TestChangedFilesSortedTrackedAndUntracked(t *testing.T) {
	root := initGitRepo(t)
	// Modify a tracked file and add two untracked files (one nested).
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "new.go"), []byte("package src\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ChangedFiles(context.Background(), root)
	want := []string{"app.go", "base.txt", "src/new.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ChangedFiles = %v, want %v (sorted)", got, want)
	}
}

// TestChangedFilesNoChanges: a clean repo reports an empty, non-nil slice.
func TestChangedFilesNoChanges(t *testing.T) {
	root := initGitRepo(t)
	got := ChangedFiles(context.Background(), root)
	if got == nil {
		t.Fatal("ChangedFiles must return a non-nil slice (JSON [] not null)")
	}
	if len(got) != 0 {
		t.Fatalf("clean repo changed files = %v, want empty", got)
	}
}

// TestChangedFilesNonGit: a non-git workspace degrades to an empty list, never a panic
// or error.
func TestChangedFilesNonGit(t *testing.T) {
	got := ChangedFiles(context.Background(), t.TempDir())
	if got == nil || len(got) != 0 {
		t.Fatalf("non-git ChangedFiles = %v, want empty non-nil slice", got)
	}
}

// TestChangedFilesIgnoresGitignored: a .gitignored untracked file is not reported.
func TestChangedFilesIgnoresGitignored(t *testing.T) {
	root := initGitRepo(t)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := ChangedFiles(context.Background(), root)
	for _, p := range got {
		if p == "ignored.txt" {
			t.Fatalf("gitignored file should not appear in %v", got)
		}
	}
}
