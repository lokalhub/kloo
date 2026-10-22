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
// spin on it: the pointer budget is bounded, and once spent the real contents come
// back.
func TestReadLedgerPointerBudgetIsBounded(t *testing.T) {
	r := newReadLedger()
	r.Observe("main.go", fileBody, 1, 0)
	for i := 0; i < maxReadPointers; i++ {
		if ptr := r.Observe("main.go", fileBody, 4+i, 0); ptr == "" {
			t.Fatalf("repeat %d should be deduped", i+1)
		}
	}
	if ptr := r.Observe("main.go", fileBody, 9, 0); ptr != "" {
		t.Fatalf("after %d pointers the contents must be served, not another pointer: %q", maxReadPointers, ptr)
	}
	// And after serving, the cycle can dedupe again rather than being disabled for
	// the rest of the run.
	if ptr := r.Observe("main.go", fileBody, 10, 0); ptr == "" {
		t.Error("ledger should re-arm after serving the contents")
	}
}

// THE READ LOOP. A model that alternates read→search→read→search on the SAME file
// never fires the consecutive-repeat rail (the calls differ), so the ledger is the
// only thing standing between it and a full dump every other turn. A one-shot latch
// let it through; the budget must hold across the interleaved turns.
func TestReadLedgerBreaksAlternatingReadSpin(t *testing.T) {
	r := newReadLedger()
	r.Observe("main.go", fileBody, 1, 0)
	dumps := 0
	for i := 0; i < 6; i++ { // read, (search), read, (search), ...
		if ptr := r.Observe("main.go", fileBody, 3+2*i, 0); ptr == "" {
			dumps++
		}
	}
	// One dump per budget window, not one every other turn: 6 alternating re-reads
	// may serve the contents at most twice (once when the budget is spent, once
	// more if the window is spent again) — never on every second turn.
	if dumps > 2 {
		t.Fatalf("dumps = %d over 6 alternating re-reads, want <= 2: the ledger must "+
			"not hand the full contents back every other turn", dumps)
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
