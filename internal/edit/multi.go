package edit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// FileResult is the per-file outcome of a multi-block Apply.
type FileResult struct {
	Path    string
	Written bool
	Err     error // nil on success; wraps ErrSearchNotFound/ErrFileExists/etc on failure
}

// Result is the outcome of applying a batch of blocks, one entry per distinct
// target file, in deterministic (path-sorted) order.
type Result struct {
	Files []FileResult
}

// Apply applies a batch of parsed blocks across one or more files with PER-FILE
// atomicity: each file's blocks are staged in memory in source order and that
// file is written exactly once, only if all of its blocks applied cleanly. If
// any block for a file fails, that file gets NO partial write (left
// byte-identical) and the failure is recorded against it; other files are
// applied and reported independently.
//
// Files are processed in a deterministic order (sorted by path), so re-running
// the same []Block yields identical writes and an equivalent Result. The
// returned error is non-nil iff at least one file failed; it joins the per-file
// errors so errors.Is(err, ErrSearchNotFound) (etc.) still holds.
//
// Cross-file (whole-batch) atomicity is intentionally NOT provided in v1 — the
// unit is per-file all-or-nothing (decisions.md). Matching is delegated to
// ApplyBlock; the new-file branch mirrors CreateFile's refuse-to-clobber rule.
func Apply(blocks []Block) (Result, error) {
	order, byFile := groupByFile(blocks)

	var res Result
	var errs []error
	for _, path := range order {
		staged, err := stageFile(path, byFile[path])
		if err != nil {
			res.Files = append(res.Files, FileResult{Path: path, Written: false, Err: err})
			errs = append(errs, err)
			continue
		}
		if err := writeStaged(path, staged); err != nil {
			res.Files = append(res.Files, FileResult{Path: path, Written: false, Err: err})
			errs = append(errs, err)
			continue
		}
		res.Files = append(res.Files, FileResult{Path: path, Written: true})
	}

	return res, errors.Join(errs...)
}

// groupByFile groups blocks by their File, returning the distinct paths in a
// deterministic (sorted) order plus the per-file blocks in source order.
func groupByFile(blocks []Block) (order []string, byFile map[string][]Block) {
	byFile = make(map[string][]Block)
	for _, b := range blocks {
		if _, seen := byFile[b.File]; !seen {
			order = append(order, b.File)
		}
		byFile[b.File] = append(byFile[b.File], b)
	}
	sort.Strings(order)
	return order, byFile
}

// stageFile computes the final content for one file by folding its blocks in
// source order, entirely in memory. It returns an error (no write should follow)
// if any block fails. The empty-SEARCH (create) form is honoured only as the
// file's first content-producing block and refuses to clobber an existing file.
func stageFile(path string, blocks []Block) (string, error) {
	staged := ""
	loaded := false

	for _, b := range blocks {
		if b.Search == "" {
			// New-file create form.
			if loaded {
				return "", fmt.Errorf("edit: %s: create block after content already staged: %w", path, ErrFileExists)
			}
			if _, err := os.Lstat(path); err == nil {
				return "", fmt.Errorf("edit: %s: %w", path, ErrFileExists)
			} else if !errors.Is(err, fs.ErrNotExist) {
				return "", fmt.Errorf("edit: stat %s: %w", path, err)
			}
			staged = b.Replace
			loaded = true
			continue
		}

		// In-place edit form.
		if !loaded {
			data, err := os.ReadFile(path)
			if err != nil {
				return "", fmt.Errorf("edit: read %s: %w", path, err)
			}
			staged = string(data)
			loaded = true
		}
		out, err := ApplyBlock(staged, b)
		if err != nil {
			return "", fmt.Errorf("edit: apply to %s: %w", path, err)
		}
		staged = out
	}

	return staged, nil
}

// writeStaged writes the staged content for a file once, creating parent dirs.
func writeStaged(path, content string) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("edit: mkdir %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(content), filePerm); err != nil {
		return fmt.Errorf("edit: write %s: %w", path, err)
	}
	return nil
}
