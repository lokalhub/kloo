package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Read de-duplication: when the model re-reads a file whose contents are STILL
// verbatim in the prompt and unchanged on disk, serve a one-line pointer instead
// of the whole dump again.
//
// The constraint that shapes this is why kloo re-reads at all. Compaction folds
// the cold middle into a running summary and replaces every file dump with
// "[read X: N lines, re-read on demand]" (memory.go summarizeCold), because a
// stored dump goes stale the moment an edit lands and the model would then reason
// against text that no longer exists on disk. Re-reading after a shed is therefore
// CORRECT — the model genuinely lost the content and needs it back. Suppressing
// that read would starve it.
//
// So this only fires in the case where the content is provably still in front of
// the model:
//
//	same path  +  identical content hash  +  no compaction since the original read
//
// The compaction check is the conservative part. The loop's convo slice always
// holds everything, but the ASSEMBLED prompt may not, and only Assemble knows what
// it shed. Rather than reach into that, this requires the compaction counter to be
// unmoved since the read was recorded — before a compaction nothing has been shed,
// so the dump is provably present. A read that MIGHT have been shed is always
// served in full.
type readMark struct {
	hash        string
	step        int
	compactions int
	// served latches the one pointer this path gets. A small local model that does
	// not understand the pointer must not be able to spin on it: the SECOND re-read
	// gets the real contents back, so the worst case is one wasted turn, not a loop.
	served bool
}

// readLedger tracks, per run, what has been read and whether its dump is still
// provably in the prompt.
type readLedger struct {
	marks map[string]readMark
}

func newReadLedger() *readLedger { return &readLedger{marks: map[string]readMark{}} }

func contentHash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// Observe records a successful read of path at the given step and compaction
// count, and reports the pointer text to substitute for the dump — or "" when the
// full contents must be served.
//
// path == "" (an unnamed read) is never deduped: without a stable key there is no
// way to prove it is the same file.
func (r *readLedger) Observe(path, content string, step, compactions int) string {
	if r == nil || path == "" {
		return ""
	}
	h := contentHash(content)
	if m, ok := r.marks[path]; ok && m.hash == h && m.compactions == compactions && !m.served {
		m.served = true
		r.marks[path] = m
		return fmt.Sprintf(
			"%s is unchanged since you read it at step %d, and its full contents are already above in this conversation — scroll up rather than re-reading. "+
				"If you need it again anyway, read it once more and the contents will be served.",
			path, m.step)
	}
	// First read, changed content, a compaction in between, or the pointer already
	// spent: record a fresh mark and serve the real thing.
	r.marks[path] = readMark{hash: h, step: step, compactions: compactions}
	return ""
}

// memCompactions reports the run's compaction count, or 0 when working memory is
// not engaged (in which case nothing is ever shed and the count never moves —
// which is exactly the condition the ledger wants).
func memCompactions(l *Loop) int {
	if l == nil || l.Memory == nil {
		return 0
	}
	return l.Memory.Stats().Compactions
}
