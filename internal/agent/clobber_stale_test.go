package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/tools"
)

// THE INCIDENT THIS FILE PINS (measured, qwen3.8-flash-next, effort medium).
//
// A run was asked to ADD two public items to desktop/src-tauri/src/services/
// stream_lens.rs, a 249-line Rust file. It read the file early, then churned on
// malformed edit_file blocks, and at step 9 fell back to write_file with 107 lines —
// deleting two working public functions (the whole HTTP transport the task depended
// on). The write SUCCEEDED. No error, no warning; the run continued for two more
// steps reporting progress, and the loss was found by hand-counting lines.
//
// The clobber guard was supposed to stop exactly this. It did not, because its
// predicate was a run-lifetime boolean: "is this path in knownFiles?". The early read
// set that flag and nothing ever unset it, so a read the model could no longer see
// counted as knowledge of the file. The fix makes the ledger remember WHEN the model
// saw a file, and treats a read that is many steps old, or on the far side of a
// compaction, as no evidence at all for a LARGE shrink.

// rustFile builds a Rust-shaped file of n lines, like the 249-line original.
func rustFile(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "    let line_%03d = compute_something(%d); // real working code\n", i, i)
	}
	return b.String()
}

// TestClobberVerdict is the table over the real shapes: a large shrink with no read,
// with a fresh read, with a stale read (by steps and by compaction), and a small
// shrink. "Fresh" must stay allowed — a model that has just read a file and means to
// rewrite it is doing legitimate work, and refusing that would break the guard's one
// legitimate case.
func TestClobberVerdict(t *testing.T) {
	const (
		nowStep        = 9 // the step the incident's write_file landed on
		nowCompactions = 1
	)
	big := rustFile(249)       // ~15 KB, the file that was destroyed
	truncated := rustFile(107) // ~6.5 KB, what was written over it (keeps ~43%)
	trimmed := rustFile(245)   // a typo-sized trim: keeps ~98%

	root := t.TempDir()
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canon, "stream_lens.rs"), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canon, "tiny.txt"), []byte("short\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(canon, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Root: canon}

	// fresh/stale marks relative to nowStep / nowCompactions.
	fresh := fileSeen{step: nowStep - 1, compactions: nowCompactions}
	staleBySteps := fileSeen{step: nowStep - (clobberStaleSteps + 1), compactions: nowCompactions}
	staleByCompaction := fileSeen{step: nowStep - 1, compactions: nowCompactions - 1}

	cases := []struct {
		name    string
		path    string
		content string
		seen    map[string]fileSeen
		want    clobberReason
	}{{
		name: "large shrink, never read -> refused as unread",
		path: "stream_lens.rs", content: truncated,
		seen: map[string]fileSeen{}, want: clobberUnread,
	}, {
		name: "large shrink, read one step ago -> allowed (an informed rewrite)",
		path: "stream_lens.rs", content: truncated,
		seen: map[string]fileSeen{"stream_lens.rs": fresh}, want: clobberOK,
	}, {
		// THE INCIDENT: read early, write at step 9, 249 lines -> 107.
		name: "large shrink, read many steps ago -> refused as stale",
		path: "stream_lens.rs", content: truncated,
		seen: map[string]fileSeen{"stream_lens.rs": staleBySteps}, want: clobberStale,
	}, {
		name: "large shrink, read before a compaction -> refused as stale",
		path: "stream_lens.rs", content: truncated,
		seen: map[string]fileSeen{"stream_lens.rs": staleByCompaction}, want: clobberStale,
	}, {
		name: "small shrink, stale read -> allowed (trimming a few lines is ordinary work)",
		path: "stream_lens.rs", content: trimmed,
		seen: map[string]fileSeen{"stream_lens.rs": staleBySteps}, want: clobberOK,
	}, {
		name: "small shrink, never read -> still refused (it cannot know what it drops)",
		path: "stream_lens.rs", content: trimmed,
		seen: map[string]fileSeen{}, want: clobberUnread,
	}, {
		name: "same-or-larger rewrite, never read -> allowed (additive, destroys nothing)",
		path: "stream_lens.rs", content: big + "// one more line\n",
		seen: map[string]fileSeen{}, want: clobberOK,
	}, {
		name: "small file, never read -> allowed (below clobberMinBytes)",
		path: "tiny.txt", content: "s\n",
		seen: map[string]fileSeen{}, want: clobberOK,
	}, {
		name: "new file -> allowed (nothing there to destroy)",
		path: "brand_new.rs", content: "fn main() {}\n",
		seen: map[string]fileSeen{}, want: clobberOK,
	}, {
		name: "a directory -> allowed (the write fails on its own terms, not here)",
		path: "adir", content: "x\n",
		seen: map[string]fileSeen{}, want: clobberOK,
	}, {
		name: "empty path -> allowed (no stable key to judge)",
		path: "", content: "x\n",
		seen: map[string]fileSeen{}, want: clobberOK,
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := l.clobberVerdict(tc.path, tc.content, tc.seen, nowStep, nowCompactions)
			if v.why != tc.want {
				t.Fatalf("clobberVerdict(%q, %d bytes over %d) = %v, want %v",
					tc.path, v.newSize, v.size, v.why, tc.want)
			}
		})
	}
}

// TestClobberCorrectionIsActionable: the refusal must TEACH. A rejection the model can
// only re-send is how the previous run burned 17 minutes producing nothing, so the
// stale nudge has to name the file, the size of the cut, when the stale read happened,
// and the exact calls to make instead — including the empty-SEARCH append form, since
// "add two items to this file" has nothing to SEARCH for.
func TestClobberCorrectionIsActionable(t *testing.T) {
	stale := clobberCheck{
		why: clobberStale, size: 15000, newSize: 6450,
		seen: fileSeen{step: 2}, seenOK: true, stepsAgo: 7,
	}
	got := buildClobberCorrection("stream_lens.rs", stale).Content
	for _, want := range []string{
		"would REPLACE the existing file stream_lens.rs (15000 bytes)",
		"6450 bytes",
		"57% cut",
		"you read stream_lens.rs at step 2",
		"7 steps ago",
		"read_file stream_lens.rs",
		"<<<<<<< SEARCH",
		">>>>>>> REPLACE",
		"empty search",
	} {
		if !strings.Contains(strings.ToLower(got), strings.ToLower(want)) {
			t.Errorf("stale clobber nudge is missing %q; got:\n%s", want, got)
		}
	}

	compacted := stale
	compacted.compacted = true
	if g := buildClobberCorrection("stream_lens.rs", compacted).Content; !strings.Contains(g, "COMPACTED") {
		t.Errorf("a compaction-stale nudge must say the conversation was compacted; got:\n%s", g)
	}

	unread := clobberCheck{why: clobberUnread, size: 15000, newSize: 6450}
	if g := buildClobberCorrection("stream_lens.rs", unread).Content; !strings.Contains(g, "have NOT read") {
		t.Errorf("an unread nudge must say the file was never read; got:\n%s", g)
	}
}

// compactAfter wraps a real WorkingMemory and reports that a compaction has happened
// once `after` turns have been assembled — the deterministic stand-in for the real
// thing, which is what put the incident's early read on the far side of a shed.
type compactAfter struct {
	WorkingMemory
	calls int
	after int
}

func (c *compactAfter) Assemble(in MemoryInput) ([]llm.Message, error) {
	c.calls++
	return c.WorkingMemory.Assemble(in)
}

func (c *compactAfter) Stats() MemoryStats {
	s := c.WorkingMemory.Stats()
	if c.calls > c.after {
		s.Compactions = 1
	}
	return s
}

// TestWriteClobberGuardBlocksShrinkAfterCompactedRead is the end-to-end reproduction:
// the model reads the 249-line file, a compaction sheds the dump, and it then writes
// 107 lines over it. The file must survive and the model must be told to re-read.
// Before the fix this test FAILED with the file truncated on disk — which is the
// incident, reproduced inside kloo's own tests.
func TestWriteClobberGuardBlocksShrinkAfterCompactedRead(t *testing.T) {
	big := rustFile(249)
	truncated := rustFile(107)
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "stream_lens.rs"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "other.rs"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{"path": "stream_lens.rs", "content": truncated}})}, // REFUSED: stale read
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "stopped"}})},
	)
	loop, root := newRealEditLoop(t, srv, "stream_lens.rs", big, nil, &stubBudget{tripAt: 100}, &stubChurn{})
	if err := os.WriteFile(filepath.Join(root, "other.rs"), []byte(rustFile(40)), 0o644); err != nil {
		t.Fatal(err)
	}
	// A compaction lands after the first two assemblies — i.e. after the read of
	// stream_lens.rs and before the write_file.
	loop.Memory = &compactAfter{WorkingMemory: NewWorkingMemory(), after: 2}
	loop.ContextTokens = 200000

	rep, err := loop.Run(context.Background(), "add two public items to stream_lens.rs")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "stream_lens.rs"))
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(string(got), "\n"); lines != 249 {
		t.Fatalf("stream_lens.rs is %d lines, want 249 — the shrinking write_file was NOT refused "+
			"and the file was destroyed (this is the incident)", lines)
	}
	if !msgWithAll(rep.Transcript, "would REPLACE the existing file", "read_file stream_lens.rs") {
		t.Error("the model was not told to re-read the file it tried to truncate")
	}
}

// TestWriteClobberGuardAllowsFreshRewrite is the other half of the gate: a model that
// read a substantial file on the PREVIOUS step and then rewrites it smaller is doing
// the legitimate thing the guard must not block.
func TestWriteClobberGuardAllowsFreshRewrite(t *testing.T) {
	big := rustFile(249)
	rewritten := rustFile(107)
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "stream_lens.rs"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"write_file", map[string]any{"path": "stream_lens.rs", "content": rewritten}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"finish", map[string]any{"summary": "rewrote it"}})},
	)
	loop, root := newRealEditLoop(t, srv, "stream_lens.rs", big, nil, &stubBudget{tripAt: 100}, &stubChurn{})

	rep, err := loop.Run(context.Background(), "simplify stream_lens.rs")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range rep.Transcript {
		if strings.Contains(m.Content, "would REPLACE the existing file") {
			t.Fatal("an informed rewrite one step after reading the file must NOT be refused")
		}
	}
	got, _ := os.ReadFile(filepath.Join(root, "stream_lens.rs"))
	if string(got) != rewritten {
		t.Error("the informed rewrite should have applied")
	}
}

// TestClobberLedgerRecordsWhenNotJustWhether guards the predicate itself: the ledger
// must carry the step/compaction at which each path was seen, so a future change
// cannot quietly go back to a run-lifetime boolean.
func TestClobberLedgerRecordsWhenNotJustWhether(t *testing.T) {
	root := t.TempDir()
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tools.NewWorkspace(canon); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canon, "f.rs"), []byte(rustFile(249)), 0o644); err != nil {
		t.Fatal(err)
	}
	l := &Loop{Root: canon}
	seen := map[string]fileSeen{"f.rs": {step: 1, compactions: 0}}

	// Same mark, same file, same content: the ONLY thing that changes is how far the
	// run has moved on. A boolean ledger cannot tell these apart.
	near := l.clobberVerdict("f.rs", rustFile(107), seen, 2, 0)
	far := l.clobberVerdict("f.rs", rustFile(107), seen, 1+clobberStaleSteps+1, 0)
	if near.why != clobberOK {
		t.Errorf("a read one step ago must stay evidence; got %v", near.why)
	}
	if far.why != clobberStale {
		t.Errorf("a read %d steps ago must NOT be evidence for a large shrink; got %v", clobberStaleSteps+1, far.why)
	}
	if far.stepsAgo != clobberStaleSteps+1 || far.seen.step != 1 {
		t.Errorf("the verdict must report WHEN the stale read was: stepsAgo=%d seen.step=%d", far.stepsAgo, far.seen.step)
	}
}
