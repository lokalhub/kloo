// Package edit is kloo's deterministic SEARCH/REPLACE edit engine. It turns a
// model's text output into real file changes: parse aider-style fenced
// SEARCH/REPLACE blocks (parse.go), apply them with EXACT matching, and FAIL
// LOUDLY on any mismatch — never fuzzy-apply, never silently write (design doc
// §8). SEARCH/REPLACE is the #1 accuracy lever for weak models: the harness, not
// the model, owns correctness here.
//
// The engine is pure-ish: ApplyBlock is in-memory and filesystem-free; the
// *File / Apply helpers do the minimal I/O. Path safety (the workspace jail)
// lives in internal/tools and wraps these functions — the engine itself trusts
// the path it is handed.
package edit

import "errors"

// Sentinel errors. Callers match these with errors.Is; wrappers add context via
// fmt.Errorf("...: %w", err).
var (
	// ErrMalformedBlock is returned by Parse when a SEARCH/REPLACE block has a
	// broken marker sequence (missing divider/terminator, unclosed fence, or no
	// filename line).
	ErrMalformedBlock = errors.New("edit: malformed SEARCH/REPLACE block")

	// ErrSearchNotFound is returned by the apply path when a block's SEARCH text
	// is not present in the target file's bytes. The file is left untouched
	// (fail-loud, no fuzzy match, no partial write).
	ErrSearchNotFound = errors.New("edit: SEARCH text not found in file")

	// ErrAmbiguousMatch is returned when a block's SEARCH text occurs more than
	// once in the target. Rather than guess which span the model meant, the
	// apply fails loudly so the harness can re-prompt with a more specific SEARCH
	// (decisions.md). The file is left untouched.
	ErrAmbiguousMatch = errors.New("edit: SEARCH text is ambiguous (matches more than once)")

	// ErrFileExists is returned by the new-file (empty-SEARCH) path when the
	// target already exists — CreateFile refuses to clobber.
	ErrFileExists = errors.New("edit: file already exists")

	// ErrEmptySearch is returned by ApplyBlock when handed an empty SEARCH on
	// existing content: the empty-SEARCH form is the new-file create path
	// (CreateFile), not an in-place edit.
	ErrEmptySearch = errors.New("edit: empty SEARCH is the new-file form")
)

// Marker lines of the aider SEARCH/REPLACE grammar (matched after trimming the
// surrounding whitespace of a line).
const (
	markerSearch  = "<<<<<<< SEARCH"
	markerDivider = "======="
	markerReplace = ">>>>>>> REPLACE"
)
