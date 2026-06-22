package tools

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/lokalhub/kloo/internal/edit"
)

// Tool name constants — the exact snake_case strings from naming.md that Phase
// 02's registry surfaces to the model.
const (
	NameReadFile  = "read_file"
	NameListDir   = "list_dir"
	NameWriteFile = "write_file"
	NameEditFile  = "edit_file"
)

// writeFilePerm is the mode for files written by write_file (matches the edit
// engine's 0644).
const writeFilePerm = 0o644

// DirEntry is one entry returned by list_dir: its name and whether it is a
// directory. No filtering is applied here (repo-map ignores are Phase 03).
type DirEntry struct {
	Name  string
	IsDir bool
}

// ReadFile is the read_file tool: it returns the content of relPath, resolved
// through the workspace jail. A missing file yields a clear error matching
// os.ErrNotExist that names the path — never a panic or a silent empty string.
func ReadFile(ws Workspace, relPath string) (string, error) {
	abs, err := ws.Resolve(relPath)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", fmt.Errorf("tools: read_file %s: %w", relPath, err)
	}
	return string(data), nil
}

// ListDir is the list_dir tool: it lists the entries of the directory at
// relPath, resolved through the jail. It filters nothing — dotfiles and
// .gitignore still appear. A missing or non-directory path yields a clear error.
func ListDir(ws Workspace, relPath string) ([]DirEntry, error) {
	abs, err := ws.Resolve(relPath)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, fmt.Errorf("tools: list_dir %s: %w", relPath, err)
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, DirEntry{Name: e.Name(), IsDir: e.IsDir()})
	}
	return out, nil
}

// WriteFile is the write_file tool: it writes content to relPath (resolved
// through the jail), creating parent directories as needed.
//
// Overwrite rule (decisions.md): write_file MAY overwrite an existing file —
// full-content replacement is its purpose. This contrasts with edit.CreateFile
// (the empty-SEARCH new-file form), which refuses to clobber. The model's
// preferred in-place edit channel is edit_file (SEARCH/REPLACE), not write_file.
func WriteFile(ws Workspace, relPath, content string) error {
	abs, err := ws.Resolve(relPath)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(abs); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("tools: write_file mkdir for %s: %w", relPath, err)
		}
	}
	if err := os.WriteFile(abs, []byte(content), writeFilePerm); err != nil {
		return fmt.Errorf("tools: write_file %s: %w", relPath, err)
	}
	return nil
}

// EditFile is the edit_file tool: the model's primary, preferred edit channel.
// It parses blockText with the Phase-01 edit engine (edit.Parse) and applies the
// blocks to relPath (resolved through the jail) via edit.Apply — it does NOT
// re-implement parsing or matching.
//
// edit_file is a single-file tool: every block in blockText is applied to
// relPath (the block's own filename line is informational; decisions.md). The
// engine's failure modes are surfaced unchanged so errors.Is still matches:
// a malformed block → edit.ErrMalformedBlock (nothing written); a SEARCH that is
// absent → edit.ErrSearchNotFound (file byte-unchanged, no fuzzy fallback).
func EditFile(ws Workspace, relPath, blockText string) error {
	abs, err := ws.Resolve(relPath)
	if err != nil {
		return err
	}
	blocks, err := edit.Parse(blockText)
	if err != nil {
		return fmt.Errorf("tools: edit_file %s: %w", relPath, err)
	}
	// Single-file tool: retarget every parsed block to the resolved path.
	for i := range blocks {
		blocks[i].File = abs
	}
	if _, err := edit.Apply(blocks); err != nil {
		return fmt.Errorf("tools: edit_file %s: %w", relPath, err)
	}
	return nil
}
