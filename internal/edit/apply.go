package edit

import (
	"fmt"
	"os"
	"strings"
)

// ApplyBlock applies a single edit Block to content in memory and returns the
// new content. Matching is EXACT: the bytes of b.Search must appear verbatim in
// content — no whitespace-insensitive, fuzzy, or closest-match logic exists
// anywhere on this path.
//
//   - If b.Search is empty, ApplyBlock returns ErrEmptySearch: the empty-SEARCH
//     form is the new-file create path (see CreateFile), not an in-place edit.
//   - If b.Search is not present in content, ApplyBlock returns ErrSearchNotFound
//     and the original content is not modified (fail-loud).
//   - Ambiguity rule (b.Search occurs more than once): REJECT with
//     ErrAmbiguousMatch rather than guess which span the model meant
//     (decisions.md). This keeps the fail-loud discipline — the harness
//     re-prompts with a more specific SEARCH instead of risking the wrong edit.
func ApplyBlock(content string, b Block) (string, error) {
	if b.Search == "" {
		return "", ErrEmptySearch
	}
	switch strings.Count(content, b.Search) {
	case 0:
		return "", ErrSearchNotFound
	case 1:
		return strings.Replace(content, b.Search, b.Replace, 1), nil
	default:
		return "", ErrAmbiguousMatch
	}
}

// ApplyToFile applies a single block to the file at path (already a resolved
// absolute path — the workspace jail is applied by the file tools, not here).
//
// The empty-SEARCH new-file form is delegated to CreateFile (task 03). For an
// in-place edit, the file is read, ApplyBlock is run, and the result is written
// back only on success — on ErrSearchNotFound (or any apply error) nothing is
// written and the on-disk file is left byte-identical.
func ApplyToFile(path string, b Block) error {
	if b.Search == "" {
		return CreateFile(path, b)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("edit: read %s: %w", path, err)
	}

	out, err := ApplyBlock(string(data), b)
	if err != nil {
		return fmt.Errorf("edit: apply to %s: %w", path, err)
	}

	if err := os.WriteFile(path, []byte(out), filePerm); err != nil {
		return fmt.Errorf("edit: write %s: %w", path, err)
	}
	return nil
}
