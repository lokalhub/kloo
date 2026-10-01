package agent

import (
	"strings"
	"testing"
)

const fileBody = "package main\n\nfunc main() {}\n"

// The first read always serves the real contents — there is nothing above to point at.
func TestReadLedgerFirstReadServesContents(t *testing.T) {
	r := newReadLedger()
	if ptr := r.Observe("main.go", fileBody, 1, 0); ptr != "" {
		t.Fatalf("first read was deduped: %q", ptr)
	}
}

// The case this exists for: same path, same bytes, no compaction in between.
func TestReadLedgerDedupesUnchangedRepeat(t *testing.T) {
	r := newReadLedger()
	r.Observe("main.go", fileBody, 1, 0)
	ptr := r.Observe("main.go", fileBody, 4, 0)
	if ptr == "" {
		t.Fatal("unchanged repeat with no compaction was not deduped")
	}
	if !strings.Contains(ptr, "main.go") || !strings.Contains(ptr, "step 1") {
		t.Errorf("pointer should name the path and the original step: %q", ptr)
	}
}

// A changed file must ALWAYS be re-served: the whole point of kloo re-reading is
// that disk is the only truth, so a stale hit here would be a correctness bug.
func TestReadLedgerServesChangedFile(t *testing.T) {
	r := newReadLedger()
	r.Observe("main.go", fileBody, 1, 0)
	if ptr := r.Observe("main.go", fileBody+"// edited\n", 4, 0); ptr != "" {
		t.Fatalf("changed file was deduped: %q", ptr)
	}
}

// THE SAFETY PROPERTY. After a compaction the dump may have been shed from the
// prompt, so the model genuinely no longer has the content. Pointing at text that
// is not there would starve it — the contents must be served in full.
func TestReadLedgerServesAfterCompaction(t *testing.T) {
	r := newReadLedger()
	r.Observe("main.go", fileBody, 1, 0)
	if ptr := r.Observe("main.go", fileBody, 9, 1); ptr != "" {
		t.Fatalf("read after a compaction was deduped — the dump may have been shed: %q", ptr)
	}
}

// THE LOOP GUARD. A model that does not understand the pointer must not be able to
// spin on it: the second re-read gets the real contents back.
func TestReadLedgerPointerIsServedOnce(t *testing.T) {
	r := newReadLedger()
	r.Observe("main.go", fileBody, 1, 0)
	if ptr := r.Observe("main.go", fileBody, 4, 0); ptr == "" {
		t.Fatal("first repeat should be deduped")
	}
	if ptr := r.Observe("main.go", fileBody, 5, 0); ptr != "" {
		t.Fatalf("second repeat must serve contents, not another pointer: %q", ptr)
	}
	// And after serving, the cycle can dedupe again rather than being disabled for
	// the rest of the run.
	if ptr := r.Observe("main.go", fileBody, 6, 0); ptr == "" {
		t.Error("ledger should re-arm after serving the contents")
	}
}

// An unnamed read has no stable key, so it can never be proven to be the same file.
func TestReadLedgerIgnoresEmptyPath(t *testing.T) {
	r := newReadLedger()
	r.Observe("", fileBody, 1, 0)
	if ptr := r.Observe("", fileBody, 2, 0); ptr != "" {
		t.Fatalf("empty path was deduped: %q", ptr)
	}
}

// Distinct paths are independent, and a nil ledger is inert rather than a panic.
func TestReadLedgerPathsAreIndependentAndNilSafe(t *testing.T) {
	r := newReadLedger()
	r.Observe("a.go", fileBody, 1, 0)
	if ptr := r.Observe("b.go", fileBody, 2, 0); ptr != "" {
		t.Fatalf("a different path was deduped off a.go's mark: %q", ptr)
	}
	var nilLedger *readLedger
	if ptr := nilLedger.Observe("a.go", fileBody, 1, 0); ptr != "" {
		t.Fatal("nil ledger should be inert")
	}
}
