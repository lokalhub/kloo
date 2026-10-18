package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// A failed run's rollback must restore ONLY what that run changed.
//
// THE INCIDENT, measured 2026-10-17 on a real app checkout. Two kloo runs were working the same
// repository on deliberately disjoint files. One finished SUCCESS at 20:21:57, its gate green.
// The other errored at 20:24:33 and logged "rolled back to checkpoint". Every file the FIRST run
// had written came back with a 20:24 mtime and its work was gone:
//
//	$ grep -rn "passwordLoginAllowed" frontend/src --include=*.ts
//	(nothing)
//	$ npx nx test frontend
//	Module '"../../services/auth.service"' has no exported member 'passwordLoginAllowed'.
//
// Rollback is `git reset --hard <checkpoint HEAD>` across the WHOLE repository, followed by
// re-applying the stash captured at checkpoint time. Anything that changed between those two
// moments and was not in that stash is destroyed. That is not limited to a second kloo run — a
// developer editing their own tree while kloo works loses those edits too, silently.
//
// The run always knows which paths it touched (the loop already keeps `editedPaths` and
// `createdPaths`), so the restore can be scoped to exactly those.

// writeIn writes content to root/rel, creating parents.
func writeIn(t *testing.T, root, rel, content string) {
	t.Helper()
	full := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, full, content)
}

func readIn(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// TestRollbackLeavesOtherWorkAlone is the incident, reduced. The run edits one file; somebody
// else writes another file after the checkpoint; the rollback must undo the first and not the
// second.
func TestRollbackLeavesOtherWorkAlone(t *testing.T) {
	root := initRepo(t, "original\n")
	writeIn(t, root, "other.txt", "committed\n")
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-m", "other")

	g := NewGitCheckpointer(root)
	snap, err := g.Checkpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// THIS run's edit.
	writeIn(t, root, "code.txt", "this run's edit\n")
	// SOMEONE ELSE's work, landed after the checkpoint. A concurrent kloo run, or the user.
	writeIn(t, root, "other.txt", "another run's finished work\n")

	if err := g.Rollback(context.Background(), snap, []string{"code.txt"}); err != nil {
		t.Fatal(err)
	}

	if got := readIn(t, root, "code.txt"); got != "original\n" {
		t.Errorf("the run's own edit was not rolled back: code.txt = %q", got)
	}
	if got := readIn(t, root, "other.txt"); got != "another run's finished work\n" {
		t.Errorf("rollback destroyed work this run never touched: other.txt = %q — this is the incident", got)
	}
}

// TestRollbackRestoresPreexistingDirtyContent: a file that was ALREADY modified when the run
// started must come back as it was at the checkpoint — not as it is in HEAD. The user's
// uncommitted work is not the run's to discard.
func TestRollbackRestoresPreexistingDirtyContent(t *testing.T) {
	root := initRepo(t, "committed\n")
	writeIn(t, root, "code.txt", "the user's uncommitted work\n")

	g := NewGitCheckpointer(root)
	snap, err := g.Checkpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeIn(t, root, "code.txt", "the run's edit on top\n")

	if err := g.Rollback(context.Background(), snap, []string{"code.txt"}); err != nil {
		t.Fatal(err)
	}
	if got := readIn(t, root, "code.txt"); got != "the user's uncommitted work\n" {
		t.Errorf("the user's pre-existing uncommitted content was not restored: %q", got)
	}
}

// TestRollbackRemovesFilesTheRunCreated: a file that did not exist at the checkpoint and is named
// in the run's paths is the run's own output and must go. The old repo-wide reset left these
// behind entirely (they are untracked), so a failed run used to leave half-written new files on
// disk.
func TestRollbackRemovesFilesTheRunCreated(t *testing.T) {
	root := initRepo(t, "original\n")
	g := NewGitCheckpointer(root)
	snap, err := g.Checkpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeIn(t, root, "pkg/new_file.go", "package pkg\n")

	if err := g.Rollback(context.Background(), snap, []string{"pkg/new_file.go"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "pkg/new_file.go")); !os.IsNotExist(err) {
		t.Error("a file this run created was left on disk after rollback")
	}
}

// TestRollbackKeepsSomeoneElsesNewFile: an untracked file that appeared after the checkpoint and
// is NOT one of the run's paths must survive. This is the other half of the incident — the
// concurrent run's brand-new files.
func TestRollbackKeepsSomeoneElsesNewFile(t *testing.T) {
	root := initRepo(t, "original\n")
	g := NewGitCheckpointer(root)
	snap, err := g.Checkpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeIn(t, root, "code.txt", "this run's edit\n")
	writeIn(t, root, "their_new_file.ts", "export const x = 1;\n")

	if err := g.Rollback(context.Background(), snap, []string{"code.txt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "their_new_file.ts")); err != nil {
		t.Errorf("rollback removed a new file this run never created: %v", err)
	}
}

// TestRollbackWithNoPathsChangesNothing: a run that edited nothing must not touch the tree at all.
// The old code still ran a repo-wide reset here, which is how a read-only run could revert a
// developer's edits.
func TestRollbackWithNoPathsChangesNothing(t *testing.T) {
	root := initRepo(t, "original\n")
	g := NewGitCheckpointer(root)
	snap, err := g.Checkpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	writeIn(t, root, "code.txt", "someone else typing while kloo reads\n")

	if err := g.Rollback(context.Background(), snap, nil); err != nil {
		t.Fatal(err)
	}
	if got := readIn(t, root, "code.txt"); got != "someone else typing while kloo reads\n" {
		t.Errorf("a no-edit run's rollback modified the tree: %q", got)
	}
}
