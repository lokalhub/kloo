package edit

import (
	"errors"
	"testing"
)

// TestParseFlexible covers the leniency that keeps small models from silent
// no-ops: a bare (unfenced) block parses like a fenced one, while a diff with no
// block at all returns zero blocks (the tool turns that into a loud error).
func TestParseFlexible(t *testing.T) {
	fenced := "a.txt\n```\n<<<<<<< SEARCH\nfoo\n=======\nbar\n>>>>>>> REPLACE\n```"
	bare := "a.txt\n<<<<<<< SEARCH\nfoo\n=======\nbar\n>>>>>>> REPLACE"

	for name, in := range map[string]string{"fenced": fenced, "unfenced": bare} {
		blocks, err := ParseFlexible(in)
		if err != nil {
			t.Fatalf("%s: ParseFlexible error: %v", name, err)
		}
		if len(blocks) != 1 || blocks[0].Search != "foo\n" || blocks[0].Replace != "bar\n" {
			t.Errorf("%s: got %+v, want one foo→bar block", name, blocks)
		}
	}

	// No block → zero blocks, no error (the caller decides it's an error).
	if blocks, err := ParseFlexible("just some prose, no markers"); err != nil || len(blocks) != 0 {
		t.Errorf("no-block input: got blocks=%d err=%v, want 0 blocks / nil err", len(blocks), err)
	}

	// A bare block that opens SEARCH but never closes is malformed, not dropped.
	if _, err := ParseFlexible("<<<<<<< SEARCH\nfoo\n======="); !errors.Is(err, ErrMalformedBlock) {
		t.Errorf("unterminated bare block should be ErrMalformedBlock, got %v", err)
	}
}
