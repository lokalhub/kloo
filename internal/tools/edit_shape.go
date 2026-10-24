package tools

import (
	"fmt"
	"os"
	"strings"

	"github.com/lokalhub/kloo/internal/edit"
)

// A REJECTION MUST TEACH, OR THE RUN BURNS.
//
// Measured (qwen3.8-flash-next, effort medium): three edit_file calls, three
// identical rejections, zero files changed, 17m11s, `run stopped — CHURN`. The
// model's analysis was correct — it knew exactly what to write. Every rejection
// read the same:
//
//	no SEARCH/REPLACE block found in diff — wrap the change as
//	<<<<<<< SEARCH / ======= / >>>>>>> REPLACE
//
// which is the grammar the tool description ALREADY carried. A model that did not
// produce the shape from the description will not produce it from the same words
// echoed back, so its retry was identical, and so was the next rejection. The test
// of an error on this path is not whether it is correct but whether a retry after
// reading it can DIFFER from the attempt that earned it.
//
// So noBlockError names what actually arrived (no markers at all, as opposed to a
// broken block, which the parser reports separately), gives both usable shapes
// verbatim, and anchors the in-place shape in a line copied out of THIS file so
// the SEARCH text is one the model can see will match.

// anchorTailLines is how many trailing lines of the target file the error quotes as a
// ready-made SEARCH anchor. Few enough to stay cheap in the observation, enough to
// be unique in a real source file.
const anchorTailLines = 3

// maxAnchorLineBytes skips a quoted anchor line longer than this: a minified
// bundle or a one-line JSON fixture would otherwise paste its entire content into
// the error.
const maxAnchorLineBytes = 200

// noBlockError builds the model-facing error for a diff that parsed to zero
// blocks. absPath is the resolved target (read to quote a real anchor); relPath is
// what the model called it, and is what the message names.
func noBlockError(relPath, absPath string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "(1) To ADD new code at the end of %s, leave the SEARCH section EMPTY:\n"+
		"<<<<<<< SEARCH\n=======\n<your new lines>\n>>>>>>> REPLACE\n", relPath)

	fmt.Fprintf(&b, "\n(2) To CHANGE lines already in %s, the SEARCH section must be lines copied from it "+
		"byte-for-byte", relPath)
	if anchor := trailingAnchor(absPath); anchor != "" {
		fmt.Fprintf(&b, ". These are its last lines, usable as-is:\n<<<<<<< SEARCH\n%s=======\n%s<your new lines>\n>>>>>>> REPLACE\n",
			anchor, anchor)
	} else {
		b.WriteString(" — read_file it first and copy the exact lines you mean to replace.\n")
	}
	// The sentinel is wrapped in the FIRST sentence, not tacked onto the end: the
	// re-prompt rail and the churn accounting match on errors.Is, and a reader
	// should not have to scroll past two code shapes to find out what went wrong.
	return fmt.Errorf("tools: edit_file %s: the diff contained no SEARCH/REPLACE markers (%w) — it looked like "+
		"plain replacement text, so nothing was changed. Re-send it in one of these two shapes.\n\n%s",
		relPath, edit.ErrMalformedBlock, b.String())
}

// trailingAnchor returns the last few lines of the file at absPath, newline
// terminated, for use as a copyable SEARCH anchor. It returns "" when the file
// cannot be read, is empty, or its tail is too long to quote — the caller then
// falls back to telling the model to read the file.
func trailingAnchor(absPath string) string {
	data, err := os.ReadFile(absPath)
	if err != nil || len(data) == 0 {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > anchorTailLines {
		lines = lines[len(lines)-anchorTailLines:]
	}
	for _, l := range lines {
		if len(l) > maxAnchorLineBytes {
			return ""
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
