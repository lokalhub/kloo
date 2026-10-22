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
//
// "Absent from the snapshot tree" is NOT the same as "did not exist": `git stash
// create` records TRACKED dirty state only, so a pre-existing UNTRACKED file is
// absent from it and very much existed. Deleting one is what destroyed
// worker/tests/error-masking.test.ts on 2026-10-21 — a run died on a transient
// 524, rolled back, and removed a file an EARLIER run had written. That is why
// Checkpoint also records the untracked set (captureUntracked) and Rollback only
// removes a path that is absent from BOTH the snapshot tree and that record.
type gitCheckpointer struct {
	root string
}

// untrackedSnapshotCap is the size above which captureUntracked records a path
// without its content. A checkpoint must never become a second copy of the
// workspace: an uncapped read next to a models directory once took kloo to 44 GB
// and got it OOM-killed. A path recorded WITHOUT content still counts as
// "existed" — it must not be deleted, it is merely not restorable.
const untrackedSnapshotCap = 1 << 20 // 1 MiB, same ceiling the repomap walk uses

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
	untracked, err := g.captureUntracked(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		Head:      strings.TrimSpace(head),
		StashRef:  strings.TrimSpace(stash),
		Untracked: untracked,
		Taken:     true,
	}, nil
}

// captureUntracked records the files that exist in the workspace but are not in
// git at checkpoint time — untracked and not .gitignored. The stash tree cannot
// answer "did this path exist before the run?" for them, so this is the record
// that lets Rollback tell "this run created it" apart from "it was already
// there, untracked".
//
// The map is workspace-relative path -> snapshot of its content. Content is held
// only for files at or below untrackedSnapshotCap; a larger file is recorded with
// the empty sentinel, which still says "existed" (so it is never deleted) but
// cannot be put back. A path that cannot be read at all is recorded the same way
// rather than dropped — dropping it would re-create the 2026-10-21 deletion.
func (g *gitCheckpointer) captureUntracked(ctx context.Context) (map[string]string, error) {
	out, err := g.git(ctx, "ls-files", "--others", "--exclude-standard")
	if err != nil {
		// Failing OPEN here (no record) is exactly the failure that deleted a
		// user's file, so a repo that cannot list its untracked files is an error
		// the caller must see rather than a silently empty snapshot.
		return nil, fmt.Errorf("agent: list untracked files: %w", err)
	}
	snap := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		p := strings.TrimSpace(line)
		if p == "" {
			continue
		}
		// Default to the no-content sentinel: only a successful read of a
		// small-enough file earns restorable content.
		snap[p] = ""
		full := filepath.Join(g.root, filepath.FromSlash(p))
		info, statErr := os.Stat(full)
		if statErr != nil || info.IsDir() || info.Size() > untrackedSnapshotCap {
			continue
		}
		data, readErr := os.ReadFile(full)
		if readErr != nil {
			continue
		}
		snap[p] = string(data)
	}
	return snap, nil
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
//   - it did NOT exist in the snapshot tree, but WAS recorded as untracked at the
//     checkpoint → it predates the run and is never deleted. With its content
//     recorded, that content is restored; recorded without content (too large to
//     snapshot) the file is left exactly as it is — we know it predates the run,
//     we cannot put it back, so we do not touch it.
//   - it did NOT exist at the checkpoint and was not untracked then → it is this
//     run's own new file, so it is removed.
//
// A path that exists now but existed at the checkpoint only in the index (a file
// this run staged) is treated as the run's own and removed too — restoring it
// from a tree that never had it would be the wrong direction. That is the one
// exception: a path the run staged is by definition not in the untracked set the
// checkpoint recorded, so it still falls in the "this run brought it here" arm.
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
			// Absent from the snapshot tree. That does NOT mean it did not exist:
			// the stash tree holds tracked state only, so check the untracked
			// record before concluding this run brought the path into existence.
			full := filepath.Join(g.root, filepath.FromSlash(p))
			if content, existed := s.Untracked[p]; existed {
				if content == "" {
					// Recorded without content: we know it predates the run, we
					// cannot put it back, so we do not touch it. Leaving whatever
					// this run wrote there is the lesser harm next to deleting
					// someone's work.
					continue
				}
				if writeErr := os.WriteFile(full, []byte(content), 0o644); writeErr != nil {
					return fmt.Errorf("agent: restore untracked %s: %w", p, writeErr)
				}
				continue
			}
			// Absent from both the snapshot tree and the untracked record ⇒ this
			// run brought it into existence. Delete it from the filesystem
			// directly: `git rm` only ever removes tracked paths, so it silently
			// leaves an untracked new file on disk, and `clean` would sweep up
			// files this run never named.
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
