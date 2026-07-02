package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// ErrNotGitRepo is returned by Checkpoint when the workspace is not a git repo —
// kloo cannot snapshot, so it warns and degrades (runs without rollback) rather
// than pretending it can roll back.
var ErrNotGitRepo = errors.New("agent: workspace is not a git repository")

// ErrNoCommits is returned when the repo has no HEAD commit to anchor a
// snapshot against (degraded, like ErrNotGitRepo).
var ErrNoCommits = errors.New("agent: git repository has no commits to checkpoint against")

// gitCheckpointer snapshots and restores the working tree via git (shelled out —
// no CGO git binding). The snapshot captures the current HEAD plus the dirty
// (tracked) working-tree state via `git stash create`, so a rollback restores
// the user's pre-existing uncommitted changes while discarding the loop's edits.
//
// Documented limitation (decisions.md): `git stash create` captures tracked
// modifications only — brand-new UNTRACKED files created during the run are not
// reverted (removing them could clobber the user's own untracked files).
type gitCheckpointer struct {
	root string
}

// NewGitCheckpointer builds a checkpointer rooted at root.
func NewGitCheckpointer(root string) *gitCheckpointer {
	return &gitCheckpointer{root: root}
}

// Checkpoint captures the current working-tree state. The loop calls this lazily
// before the first edit (read-only runs take none).
func (g *gitCheckpointer) Checkpoint(ctx context.Context) (Snapshot, error) {
	if out, err := g.git(ctx, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return Snapshot{}, ErrNotGitRepo
	}
	head, err := g.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return Snapshot{}, ErrNoCommits
	}
	// stash create captures the dirty tracked tree without touching it; empty
	// output means the tree is clean.
	stash, err := g.git(ctx, "stash", "create", "kloo-checkpoint")
	if err != nil {
		return Snapshot{}, fmt.Errorf("agent: git stash create: %w", err)
	}
	return Snapshot{
		Head:     strings.TrimSpace(head),
		StashRef: strings.TrimSpace(stash),
		Taken:    true,
	}, nil
}

// Rollback restores the working tree to the snapshot: hard-reset tracked files to
// the checkpoint HEAD (discarding the loop's edits), then re-apply the captured
// pre-existing dirty changes. Safe to call once per terminal path; idempotent.
func (g *gitCheckpointer) Rollback(ctx context.Context, s Snapshot) error {
	if !s.Taken {
		return nil // nothing was snapshotted (e.g. non-git workspace)
	}
	if _, err := g.git(ctx, "reset", "--hard", s.Head); err != nil {
		return fmt.Errorf("agent: git reset --hard %s: %w", s.Head, err)
	}
	if s.StashRef != "" {
		if _, err := g.git(ctx, "stash", "apply", s.StashRef); err != nil {
			return fmt.Errorf("agent: git stash apply %s: %w", s.StashRef, err)
		}
	}
	return nil
}

// ChangedFiles returns the workspace-relative paths changed in root's working tree
// relative to HEAD (tracked modifications, staged or unstaged) plus new untracked
// files (excluding .gitignored ones), sorted and de-duplicated for deterministic
// benchmark accounting (B3). A non-git workspace, or a repo with no HEAD, degrades to
// what it can report (often an empty list) rather than failing — accounting is
// best-effort, never a run gate. The returned slice is non-nil (empty, not null).
func ChangedFiles(ctx context.Context, root string) []string {
	g := &gitCheckpointer{root: root}
	paths := []string{}
	if out, err := g.git(ctx, "rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(out) != "true" {
		return paths
	}
	set := map[string]bool{}
	addLines := func(out string) {
		for _, line := range strings.Split(out, "\n") {
			if p := strings.TrimSpace(line); p != "" {
				set[p] = true
			}
		}
	}
	// Tracked changes vs HEAD (both staged and unstaged). Skipped silently when the
	// repo has no commits (rev-parse HEAD would fail) — untracked detection still runs.
	if out, err := g.git(ctx, "diff", "--name-only", "HEAD"); err == nil {
		addLines(out)
	}
	// New files not yet tracked, honouring .gitignore.
	if out, err := g.git(ctx, "ls-files", "--others", "--exclude-standard"); err == nil {
		addLines(out)
	}
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// git runs a git command in the workspace root with a stable identity (so object
// creation never fails on a repo lacking a configured user).
func (g *gitCheckpointer) git(ctx context.Context, args ...string) (string, error) {
	full := append([]string{
		"-C", g.root,
		"-c", "user.name=kloo-agent",
		"-c", "user.email=kloo@localhost",
	}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
