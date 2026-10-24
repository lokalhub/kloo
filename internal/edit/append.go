package edit

import (
	"fmt"
	"os"
	"strings"
)

// THE APPEND FORM, AND WHY IT EXISTS.
//
// Measured (qwen3.8-flash-next, effort medium): a run asked to ADD two new public
// items to an existing Rust file died with `run stopped — CHURN`, failed_edits=3,
// ZERO files changed, after 17m11s. Its final answer showed the analysis was
// correct and complete — it knew exactly what to write. All three edit_file calls
// were rejected with the identical "no SEARCH/REPLACE block found in diff".
//
// The cause is structural, not random: WHEN THE TASK IS TO APPEND, THERE IS
// NOTHING TO SEARCH FOR. The engine's grammar could express "replace these exact
// lines" and "create this file", but not "add this at the end" — and the one shape
// that looks like it should mean that, an empty SEARCH section, meant *create* and
// therefore failed with ErrFileExists on the file the model was editing. A weak
// model reaches for "here is the new code", gets rejected, re-sends the same shape
// and burns the budget.
//
// So the empty-SEARCH rule is now uniform and teachable, which is the only kind a
// small model can follow: EMPTY SEARCH = ADD Replace AT THE END, creating the file
// if it does not exist.
//
// WHY THIS DOES NOT WEAKEN ANY SAFETY RAIL. The fail-loud discipline of §8 is
// about never writing content the model did not intend and never destroying
// content it never saw. An append does neither: it is strictly additive, so no
// byte of the existing file is read, moved or lost, and it is the OPPOSITE of the
// write_file clobber guard's concern (a blind shrinking overwrite). The one way an
// append can mislead is by landing twice — exactly what a churning model would do
// — so appendTo refuses when the text is already in the file verbatim
// (ErrAlreadyPresent) and leaves it byte-identical.
//
// The create-collision case does change: a model that meant to CREATE a file that
// already exists now appends to it instead of getting ErrFileExists. That is the
// deliberate trade. ErrFileExists was a dead end that taught the model nothing it
// could act on, while a wrong append is additive, visible in the diff, caught by
// the verify gate, and recoverable with git — a far cheaper failure than 17
// minutes of churn producing nothing at all.

// AppendToFile implements the append (empty-SEARCH on an EXISTING file) form: it
// adds b.Replace to the end of the file at path, guaranteeing a newline boundary
// between the old content and the new.
//
// It refuses with ErrAlreadyPresent when b.Replace is already in the file
// verbatim, writing nothing. The file must exist; creating one is CreateFile's
// job, and ApplyToFile routes between the two.
func AppendToFile(path string, b Block) error {
	data, err := readFileCapped(path)
	if err != nil {
		return fmt.Errorf("edit: read %s: %w", path, err)
	}
	out, err := appendTo(string(data), b.Replace)
	if err != nil {
		return fmt.Errorf("edit: append to %s: %w", path, err)
	}
	if err := os.WriteFile(path, []byte(out), filePerm); err != nil {
		return fmt.Errorf("edit: write %s: %w", path, err)
	}
	return nil
}

// appendTo is the in-memory half of the append form: the pure function that both
// ApplyToFile and the batch stager fold through, so the single-block and
// multi-block paths can never disagree about what an append produces.
//
// The newline boundary is inserted only when content is non-empty and does not
// already end in "\n" — appending to an empty file must not leave a leading blank
// line, and appending to a normally-terminated file must not leave a blank line
// between the old last line and the new first one.
func appendTo(content, add string) (string, error) {
	if add != "" && strings.Contains(content, add) {
		return "", ErrAlreadyPresent
	}
	if content == "" || strings.HasSuffix(content, "\n") {
		return content + add, nil
	}
	return content + "\n" + add, nil
}
