package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// (tracked) working-tree state via `git stash create`, so a restore can put back
// the user's pre-existing uncommitted changes rather than HEAD's version.
//
// The restore is SCOPED to the paths the run named (see Rollback): each named
// path is put back as it was at the checkpoint, and a named path that did not
// exist then — a file this run created — is removed. Nothing outside that list
// is touched, and there is no repo-wide reset.
//
// Consequence of scoping (decisions.md): a file this run created IS removed,
// while anyone else's new untracked files survive. A repo-wide `git reset
// --hard` used to do the opposite — it kept the run's half-written new files and
// destroyed everyone else's committed-then-edited work (the 2026-10-17 incident).
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

// Rollback restores ONLY the named paths to their checkpoint state. Safe to call
// once per terminal path; idempotent.
//
// For each path:
//   - it existed at the checkpoint → restore THAT content. The stash commit
//     created by `git stash create` has a tree equal to the working tree at
//     checkpoint time, so checking the path out of the stash ref restores the
//     user's pre-existing uncommitted content, not HEAD's. With a clean tree at
//     checkpoint there is no stash ref and HEAD is the correct source.
//   - it did NOT exist at the checkpoint → it is this run's own new file, so it
//     is removed.
//
// A path that exists now but existed at the checkpoint only in the index (a file
// this run staged) is treated as the run's own and removed too — restoring it
// from a tree that never had it would be the wrong direction.
//
// It NEVER runs `git reset --hard` and NEVER touches a path outside `paths`: a
// repo-wide restore wipes whatever landed after the checkpoint, which destroyed
// a concurrent run's finished work on 2026-10-17. An empty or nil `paths`
// therefore changes nothing at all.
func (g *gitCheckpointer) Rollback(ctx context.Context, s Snapshot, paths []string) error {
	if !s.Taken || len(paths) == 0 {
		return nil // nothing snapshotted (non-git workspace), or nothing this run changed
	}
	// The stash commit's tree IS the working tree at checkpoint time; with a
	// clean checkpoint there was nothing to stash and HEAD is that tree.
	src := s.StashRef
	if src == "" {
		src = s.Head
	}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := g.git(ctx, "cat-file", "-e", src+":"+p); err != nil {
			// Absent at the checkpoint ⇒ this run brought it into existence.
			// Delete it from the filesystem directly: `git rm` only ever removes
			// tracked paths, so it silently leaves an untracked new file on disk,
			// and `clean` would sweep up files this run never named.
			full := filepath.Join(g.root, filepath.FromSlash(p))
			if rmErr := os.Remove(full); rmErr != nil && !os.IsNotExist(rmErr) {
				return fmt.Errorf("agent: remove %s: %w", p, rmErr)
			}
			continue
		}
		if _, err := g.git(ctx, "checkout", src, "--", p); err != nil {
			return fmt.Errorf("agent: restore %s from %s: %w", p, src, err)
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
