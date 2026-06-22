package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func haveGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
}

// initRepo creates a temp git repo with code.txt committed as content, and
// returns the repo root.
func initRepo(t *testing.T, content string) string {
	t.Helper()
	haveGit(t)
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	writeFile(t, filepath.Join(root, "code.txt"), content)
	run("add", "-A")
	run("commit", "-m", "seed")
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCheckpointCleanRepoRollback(t *testing.T) {
	root := initRepo(t, "v1\n")
	cp := NewGitCheckpointer(root)
	ctx := context.Background()

	snap, err := cp.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if !snap.Taken {
		t.Fatal("snapshot should be taken")
	}

	// Loop edit.
	writeFile(t, filepath.Join(root, "code.txt"), "loop-edit\n")

	if err := cp.Rollback(ctx, snap); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "code.txt")); got != "v1\n" {
		t.Errorf("clean rollback should restore committed state, got %q", got)
	}
}

func TestCheckpointDirtyRepoPreservesUserChanges(t *testing.T) {
	root := initRepo(t, "v1\n")
	cp := NewGitCheckpointer(root)
	ctx := context.Background()

	// Pre-existing UNCOMMITTED user change present at checkpoint time.
	writeFile(t, filepath.Join(root, "code.txt"), "user-dirty\n")

	snap, err := cp.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	// Loop edit on top.
	writeFile(t, filepath.Join(root, "code.txt"), "loop-edit\n")

	if err := cp.Rollback(ctx, snap); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// The user's pre-existing change survives; the loop edit is gone.
	if got := readFile(t, filepath.Join(root, "code.txt")); got != "user-dirty\n" {
		t.Errorf("dirty rollback should preserve user change + drop loop edit, got %q", got)
	}
}

func TestCheckpointNotAGitRepo(t *testing.T) {
	root := t.TempDir() // not a git repo
	cp := NewGitCheckpointer(root)
	_, err := cp.Checkpoint(context.Background())
	if !errors.Is(err, ErrNotGitRepo) {
		t.Errorf("want ErrNotGitRepo (degraded, never a fake snapshot), got %v", err)
	}
}

func TestRollbackIdempotent(t *testing.T) {
	root := initRepo(t, "v1\n")
	cp := NewGitCheckpointer(root)
	ctx := context.Background()

	snap, _ := cp.Checkpoint(ctx)
	writeFile(t, filepath.Join(root, "code.txt"), "loop-edit\n")

	if err := cp.Rollback(ctx, snap); err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	if err := cp.Rollback(ctx, snap); err != nil {
		t.Fatalf("second rollback should be safe: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "code.txt")); got != "v1\n" {
		t.Errorf("idempotent rollback content = %q", got)
	}
}

func TestRollbackUntakenSnapshotIsNoop(t *testing.T) {
	cp := NewGitCheckpointer(t.TempDir())
	if err := cp.Rollback(context.Background(), Snapshot{Taken: false}); err != nil {
		t.Errorf("rollback of an untaken snapshot should be a no-op, got %v", err)
	}
}
