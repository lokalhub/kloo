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

	if err := cp.Rollback(ctx, snap, []string{"code.txt"}); err != nil {
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

	if err := cp.Rollback(ctx, snap, []string{"code.txt"}); err != nil {
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

	if err := cp.Rollback(ctx, snap, []string{"code.txt"}); err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	if err := cp.Rollback(ctx, snap, []string{"code.txt"}); err != nil {
		t.Fatalf("second rollback should be safe: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "code.txt")); got != "v1\n" {
		t.Errorf("idempotent rollback content = %q", got)
	}
}

func TestRollbackUntakenSnapshotIsNoop(t *testing.T) {
	cp := NewGitCheckpointer(t.TempDir())
	if err := cp.Rollback(context.Background(), Snapshot{Taken: false}, []string{"code.txt"}); err != nil {
		t.Errorf("rollback of an untaken snapshot should be a no-op, got %v", err)
	}
}

// gitIn runs a git command inside root, failing the test on error.
func gitIn(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// fileExists reports whether path is on disk (a rollback must not have removed it).
func fileExists(t *testing.T, path string) bool {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatalf("stat %s: %v", path, err)
	}
	return true
}

// TestRollbackKeepsPreexistingUntrackedFile pins the 2026-10-21 incident: a file
// written by an EARLIER run, sitting on disk untracked before this run started,
// was DELETED by the rollback. `git stash create` records tracked state only, so
// the file was absent from the snapshot tree and Rollback read that as "this run
// created it". The checkpoint now also records the untracked set, so the file is
// put back exactly as it was rather than removed.
func TestRollbackKeepsPreexistingUntrackedFile(t *testing.T) {
	root := initRepo(t, "v1\n")
	ctx := context.Background()

	// A file from an earlier run: on disk before the checkpoint, untracked.
	legacy := filepath.Join(root, "tests", "error-masking.test.ts")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, legacy, "original from an earlier run\n")

	cp := NewGitCheckpointer(root)
	snap, err := cp.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if _, ok := snap.Untracked["tests/error-masking.test.ts"]; !ok {
		t.Fatalf("checkpoint must record the pre-existing untracked file, got %v", snap.Untracked)
	}

	// This run edits that file, then dies on a transient model-endpoint 524.
	writeFile(t, legacy, "half-written by this run\n")

	if err := cp.Rollback(ctx, snap, []string{"tests/error-masking.test.ts"}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !fileExists(t, legacy) {
		t.Fatal("rollback deleted a file that existed before the run")
	}
	if got := readFile(t, legacy); got != "original from an earlier run\n" {
		t.Errorf("rollback should restore the pre-run content, got %q", got)
	}
}

// TestRollbackKeepsPreexistingUntrackedFileWithoutContent covers the bounded
// snapshot: a file above the size cap is recorded by PATH ONLY, with no content
// to restore. We know it predates the run, we cannot put it back, so we do not
// touch it — it must still survive the rollback.
func TestRollbackKeepsPreexistingUntrackedFileWithoutContent(t *testing.T) {
	root := initRepo(t, "v1\n")
	cp := NewGitCheckpointer(root)
	ctx := context.Background()

	snap, err := cp.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	// Simulate the oversized-file arm of captureUntracked: path recorded, no
	// content captured (the file is on disk but not restorable).
	snap.Untracked["big.dat"] = ""

	big := filepath.Join(root, "big.dat")
	writeFile(t, big, "whatever this run wrote\n")

	if err := cp.Rollback(ctx, snap, []string{"big.dat"}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if !fileExists(t, big) {
		t.Fatal("rollback deleted a file recorded as pre-existing (even without content)")
	}
	if got := readFile(t, big); got != "whatever this run wrote\n" {
		t.Errorf("an unrestorable pre-existing file must be left exactly as it is, got %q", got)
	}
}

// TestRollbackRemovesFileCreatedDuringRun is the other half of the property, and
// the part that must keep working: a path absent from BOTH the snapshot tree and
// the untracked record is this run's own new file, so it is removed.
func TestRollbackRemovesFileCreatedDuringRun(t *testing.T) {
	root := initRepo(t, "v1\n")
	cp := NewGitCheckpointer(root)
	ctx := context.Background()

	snap, err := cp.Checkpoint(ctx)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if _, ok := snap.Untracked["newfile.go"]; ok {
		t.Fatal("a file the run has not created yet must not be in the untracked record")
	}

	writeFile(t, filepath.Join(root, "newfile.go"), "created by this run\n")

	if err := cp.Rollback(ctx, snap, []string{"newfile.go"}); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if fileExists(t, filepath.Join(root, "newfile.go")) {
		t.Error("a file this run created must still be removed on rollback")
	}
}

// TestCaptureUntrackedRecordsPathsAndContent pins the recorder itself: untracked
// non-ignored files are listed with their content, an over-cap file is recorded
// without content (still proving it existed), and a .gitignored file is not.
func TestCaptureUntrackedRecordsPathsAndContent(t *testing.T) {
	root := initRepo(t, "v1\n")
	writeFile(t, filepath.Join(root, ".gitignore"), "ignored.txt\n")
	writeFile(t, filepath.Join(root, "loose.txt"), "loose content\n")
	writeFile(t, filepath.Join(root, "ignored.txt"), "should not be recorded\n")
	oversize := filepath.Join(root, "huge.bin")
	if err := os.WriteFile(oversize, make([]byte, untrackedSnapshotCap+1), 0o644); err != nil {
		t.Fatal(err)
	}

	cp := NewGitCheckpointer(root)
	got, err := cp.captureUntracked(context.Background())
	if err != nil {
		t.Fatalf("captureUntracked: %v", err)
	}
	if got["loose.txt"] != "loose content\n" {
		t.Errorf("untracked file content = %q, want the file's bytes", got["loose.txt"])
	}
	if _, ok := got["ignored.txt"]; ok {
		t.Error("a .gitignored file must not be recorded as untracked")
	}
	content, ok := got["huge.bin"]
	if !ok {
		t.Fatal("an over-cap file must still be recorded by path — it existed")
	}
	if content != "" {
		t.Error("an over-cap file must be recorded WITHOUT its content (bounded snapshot)")
	}
}
