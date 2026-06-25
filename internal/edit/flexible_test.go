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

// TestParseFlexibleReplaceAsDivider: a weak model (e.g. gpt-oss) omits the
// "=======" divider and writes ">>>>>>> REPLACE" in its place, then the replace
// body with no closing marker. The lenient parser recovers it (treats REPLACE as
// the divider) so the edit applies instead of failing as malformed.
func TestParseFlexibleReplaceAsDivider(t *testing.T) {
	// SEARCH / (no =======) / >>>>>>> REPLACE used as divider / replace body, no close.
	in := "<<<<<<< SEARCH\nold line 1\nold line 2\n>>>>>>> REPLACE\nnew line 1\nnew line 2\n"
	blocks, err := ParseFlexible(in)
	if err != nil {
		t.Fatalf("expected lenient recovery, got error: %v", err)
	}
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if blocks[0].Search != "old line 1\nold line 2\n" {
		t.Errorf("Search = %q", blocks[0].Search)
	}
	if blocks[0].Replace != "new line 1\nnew line 2\n" {
		t.Errorf("Replace = %q (trailing newline should not double)", blocks[0].Replace)
	}

	// A well-formed block (======= present) is UNCHANGED by the leniency.
	ok := "<<<<<<< SEARCH\nfoo\n=======\nbar\n>>>>>>> REPLACE"
	b, err := ParseFlexible(ok)
	if err != nil || len(b) != 1 || b[0].Search != "foo\n" || b[0].Replace != "bar\n" {
		t.Errorf("well-formed block changed by leniency: %+v err=%v", b, err)
	}
}
