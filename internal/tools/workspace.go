// Package tools holds kloo's agent-facing file tools (read_file, list_dir,
// write_file, edit_file) and the canonical workspace path-jail that confines
// every one of them to a single workspace root.
//
// The model is untrusted input: it must never be able to read or write outside
// the workspace. Workspace.Resolve is the single chokepoint every file tool
// passes its path through; Phase 02 extends this package with the tool registry,
// adapters, and run_command, reusing this same jail (no second/forked jail).
package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrPathEscape is returned when a path resolves outside the workspace root
// (via ../ traversal, an absolute path, or a symlink pointing out).
var ErrPathEscape = errors.New("tools: path escapes workspace root")

// Workspace confines file access to a single root directory. The root is stored
// as a cleaned, absolute, symlink-evaluated canonical path so containment checks
// are exact.
type Workspace struct {
	root string
}

// NewWorkspace canonicalises root (absolute + symlink-resolved) once and returns
// a Workspace jailed to it. The root must exist.
func NewWorkspace(root string) (Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Workspace{}, fmt.Errorf("tools: resolve workspace root: %w", err)
	}
	// Canonicalise the root through symlinks so later containment checks compare
	// like with like (the root itself may sit under a symlinked path).
	canon, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return Workspace{}, fmt.Errorf("tools: canonicalise workspace root %s: %w", abs, err)
	}
	return Workspace{root: filepath.Clean(canon)}, nil
}

// Root returns the canonical workspace root.
func (w Workspace) Root() string { return w.root }

// Resolve maps a tool's path (relative to the workspace, or absolute) to an
// absolute path inside the jail, or returns ErrPathEscape. It is the single
// chokepoint: file tools must use only its returned path, never the raw input.
//
// The check is twofold: (1) lexical — clean the joined path and require it to be
// within root; (2) symlink — resolve symlinks on the longest existing ancestor
// and require the result to remain within root, so a symlink in an existing
// parent cannot redirect the write outside the jail. The returned path is the
// lexical (un-evaluated) path so writes land at the intended location.
//
// Symlink rule (decisions.md): a symlink whose target is outside root is
// rejected; one pointing inside root is allowed. Caveat: the check is subject to
// TOCTOU — a symlink swapped between Resolve and the subsequent open could defeat
// it; v1 accepts this (single local user, no concurrent adversary).
func (w Workspace) Resolve(relPath string) (string, error) {
	joined := relPath
	if !filepath.IsAbs(joined) {
		joined = filepath.Join(w.root, relPath)
	}
	clean := filepath.Clean(joined)

	if !w.contains(clean) {
		return "", fmt.Errorf("tools: %q: %w", relPath, ErrPathEscape)
	}

	evaled, err := evalExistingAncestor(clean)
	if err != nil {
		return "", fmt.Errorf("tools: resolve %q: %w", relPath, err)
	}
	if !w.contains(evaled) {
		return "", fmt.Errorf("tools: %q (via symlink): %w", relPath, ErrPathEscape)
	}

	return clean, nil
}

// contains reports whether p is the root itself or lies beneath it.
func (w Workspace) contains(p string) bool {
	if p == w.root {
		return true
	}
	return strings.HasPrefix(p, w.root+string(os.PathSeparator))
}

// evalExistingAncestor resolves symlinks on the longest existing prefix of p and
// re-appends the (not-yet-existing) tail, so symlink containment can be checked
// even for paths that don't exist yet (writes / new files).
func evalExistingAncestor(p string) (string, error) {
	cur := p
	var tail string
	for {
		if _, err := os.Lstat(cur); err == nil {
			resolved, err := filepath.EvalSymlinks(cur)
			if err != nil {
				return "", err
			}
			return filepath.Join(resolved, tail), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the filesystem root without finding an existing ancestor;
			// nothing to symlink-resolve.
			return p, nil
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}
