package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/repomap"
	"github.com/lokalhub/kloo/internal/tools"
)

// Working-memory budget fractions and caps. They are named consts (greppable)
// so P01's contextProfile can later scale them per model; for P00 they are
// fixed. The 70% compaction trigger is taken verbatim from overview §3, and the
// component caps (map + hot) sum to it deliberately, leaving headroom between the
// trigger and the hard ceiling (1.0 × window) for the running-summary slot.
const (
	compactTriggerFrac = 0.70 // projected prompt > this × window ⇒ start a compaction
	mapBudgetFrac      = 0.35 // repo-map section cap (was 100% — the "map eats the window" bug)
	hotBudgetFrac      = 0.35 // pin-hot set + recent tail cap
	maxKeepItemTokens  = 256  // per-item verbatim cap; a larger kept item is truncated-with-marker
	ceilSlack          = 4    // tokens reserved when truncating, to absorb ApproxTokens rounding so the hard ceiling holds strictly
)

// mapBudgetTokens is the repo-map token budget when working memory is engaged —
// a fraction of the window, so the map can no longer consume the whole context.
func mapBudgetTokens(window int) int { return int(float64(window) * mapBudgetFrac) }

// hotBudgetTokens caps the pin-hot set + recent tail (verbatim hot state).
func hotBudgetTokens(window int) int { return int(float64(window) * hotBudgetFrac) }

// summaryPrefix labels the running-summary slot inserted right after the task.
const summaryPrefix = "Progress so far (compacted):\n"

// workingMemory is the deterministic in-process WorkingMemory. It assembles the
// pin-hot set (task, last verify, current file, recent tail) under the budget
// split and folds the cold middle into a running summary when the projected
// prompt crosses the 70% trigger. No LLM call: every decision is structural, so
// the assembly is byte-for-byte testable and adds no latency.
type workingMemory struct {
	stats       MemoryStats
	compactions int // cumulative across the run (overview §3: the ⟲ counter)
}

// NewWorkingMemory builds the deterministic in-process working memory. It takes
// no config in P00 (the fractions are fixed consts); P01's contextProfile will
// add per-model scaling here.
func NewWorkingMemory() *workingMemory { return &workingMemory{} }

// Stats returns the last assembly's accounting.
func (w *workingMemory) Stats() MemoryStats { return w.stats }

// Assemble builds the per-request history under the window budget: the pin-hot
// set + recent tail verbatim, with the cold middle folded into a running summary
// once the projected prompt crosses the 70% soft trigger, then shed in a fixed
// order to honor the hard ceiling (1.0 × window). convo[0] (the task) is never
// dropped; if the window cannot even hold system+task, it returns
// ErrWindowTooSmall (§2.4).
func (w *workingMemory) Assemble(in MemoryInput) ([]llm.Message, error) {
	window := in.WindowTokens

	// Irreducible floor: the already-built system prompt (base + repo map) plus
	// the task message, which is never dropped. If the window cannot hold even
	// that, there is no honest way to honor the hard ceiling — surface a config
	// error rather than emit an over-ceiling prompt or drop the goal (§2.4).
	floor := in.SystemTokens + repomap.ApproxTokens(in.Task)
	if window > 0 && floor > window {
		w.stats = MemoryStats{WindowTokens: window, Compactions: w.compactions, MapBudget: in.MapBudget, HotBudget: hotBudgetTokens(window)}
		return nil, ErrWindowTooSmall
	}

	hotBudget := hotBudgetTokens(window)
	task := llm.Message{Role: llm.RoleUser, Content: in.Task}
	vp, hasVerify := verifyPin(in.LastVerify)
	fp, hasFile := filePin(in.EditPath, in.FreshFile)

	// Recent tail: prior-session turns (oldest) followed by this run's transcript
	// after the task, minus any stale read-dump of the file currently under edit
	// (it is re-read fresh into the file pin). Seeding History as the oldest tail
	// means a follow-up has the prior run's context, and it's the first thing the
	// compactor folds into the summary under window pressure — while the current
	// task (in.Task) stays pinned regardless.
	allTail := append(append([]llm.Message{}, in.History...), recentTail(in.Convo, in.EditPath)...)

	// Pin tokens (everything pinned besides the task) — they share the hot budget
	// with the tail.
	pinMsgs := []llm.Message{}
	if hasVerify {
		pinMsgs = append(pinMsgs, vp)
	}
	if hasFile {
		pinMsgs = append(pinMsgs, fp)
	}
	pinTokens := tokensOf(pinMsgs)

	// No-compaction fast path: if the whole transcript projects under the soft
	// trigger (or the window is unbounded), keep everything verbatim — the
	// manager steps aside (overview §1: don't get in the way when there's room).
	projectedFull := in.SystemTokens + repomap.ApproxTokens(in.Task) + pinTokens + tokensOf(allTail)
	if window <= 0 || projectedFull <= triggerTokens(window) {
		out := assemble(task, nil, pinMsgs, allTail)
		w.stats = MemoryStats{
			PromptTokens: in.SystemTokens + tokensOf(out),
			WindowTokens: window,
			Compactions:  w.compactions,
			MapBudget:    in.MapBudget,
			HotBudget:    hotBudget,
		}
		return out, nil
	}

	// ── Compaction ────────────────────────────────────────────────────────────
	// Keep the newest turns verbatim within the hot budget; fold the rest (the
	// cold middle) into the running-summary slot via structural extraction.
	tailBudget := hotBudget - pinTokens
	tail, _ := keepTailWithin(allTail, tailBudget)
	cold := allTail[:len(allTail)-len(tail)]
	entries := summarizeCold(cold, maxKeepItemTokens)
	w.compactions++

	// Shed in the fixed order until the hard ceiling holds (§2.4): the task and
	// system base are the floor and never shed; window ≥ floor guarantees
	// termination.
	tailTrimmed := false
	rebuildPins := func() []llm.Message {
		p := []llm.Message{}
		if hasVerify {
			p = append(p, vp)
		}
		if hasFile {
			p = append(p, fp)
		}
		return p
	}
	projected := func() int {
		return in.SystemTokens + tokensOf(assemble(task, entries, rebuildPins(), tail))
	}
	// fixedTokens is everything that is NOT the named component, so a component's
	// available budget is window − everything-else.
	fixedExcept := func(skipSummary, skipFile, skipVerify bool) int {
		t := in.SystemTokens + repomap.ApproxTokens(in.Task) + tokensOf(tail)
		if !skipSummary {
			t += summaryTokens(entries)
		}
		if hasFile && !skipFile {
			t += repomap.ApproxTokens(fp.Content)
		}
		if hasVerify && !skipVerify {
			t += repomap.ApproxTokens(vp.Content)
		}
		return t
	}

	// (2) shrink the running summary, oldest entry first.
	for projected() > window && len(entries) > 1 {
		entries = entries[1:]
	}
	// (2b) one entry still too big ⇒ truncate it head/tail-with-marker so the
	// summary fits (resolves B6: a single oversized item is never dropped whole).
	if projected() > window && len(entries) == 1 {
		avail := window - fixedExcept(true, false, false) - repomap.ApproxTokens(summaryPrefix) - ceilSlack
		if t, _ := truncateToTokens(entries[0], avail); t != "" {
			entries[0] = t
		} else {
			entries = nil
		}
	}
	// (3) shrink the recent tail, oldest turn first.
	for projected() > window && len(tail) > 0 {
		tail = tail[1:]
		tailTrimmed = true
	}
	// (4) truncate the current-file pin (re-read from disk next turn, recoverable).
	if projected() > window && hasFile {
		avail := window - fixedExcept(false, true, false) - ceilSlack
		if t, _ := truncateToTokens(fp.Content, avail); t != "" {
			fp.Content = t
		} else {
			hasFile = false
		}
	}
	// (5) truncate the last-verify tail.
	if projected() > window && hasVerify {
		avail := window - fixedExcept(false, false, true) - ceilSlack
		if t, _ := truncateToTokens(vp.Content, avail); t != "" {
			vp.Content = t
		} else {
			hasVerify = false
		}
	}

	out := assemble(task, entries, rebuildPins(), tail)
	w.stats = MemoryStats{
		PromptTokens:  in.SystemTokens + tokensOf(out),
		WindowTokens:  window,
		Compactions:   w.compactions,
		SummaryTokens: summaryTokens(entries),
		DroppedTurns:  len(cold),
		TrimmedTail:   tailTrimmed,
		MapBudget:     in.MapBudget,
		HotBudget:     hotBudget,
	}
	return out, nil
}

// assemble lays out the per-request messages in prompt order: task, then the
// running-summary slot (right after the task), then the pins, then the tail.
func assemble(task llm.Message, summaryEntries []string, pins, tail []llm.Message) []llm.Message {
	out := make([]llm.Message, 0, 2+len(pins)+len(tail))
	out = append(out, task)
	if len(summaryEntries) > 0 {
		out = append(out, llm.Message{Role: llm.RoleUser, Content: summaryPrefix + strings.Join(summaryEntries, "\n")})
	}
	out = append(out, pins...)
	out = append(out, tail...)
	return out
}

// triggerTokens is the soft compaction trigger: compactTriggerFrac × window.
func triggerTokens(window int) int { return int(compactTriggerFrac * float64(window)) }

// summaryTokens is the token cost of the summary slot for the given entries
// (0 when there are none).
func summaryTokens(entries []string) int {
	if len(entries) == 0 {
		return 0
	}
	return repomap.ApproxTokens(summaryPrefix + strings.Join(entries, "\n"))
}

// tokensOf sums the approximate token count over the messages' content.
func tokensOf(msgs []llm.Message) int {
	n := 0
	for _, m := range msgs {
		n += repomap.ApproxTokens(m.Content)
	}
	return n
}

// verifyPin synthesizes the pinned last-verify message: the verify command, the
// pass/fail + exit code, and (on failure) the failing output verbatim — the one
// signal the loop trusts, kept whole so the model sees exactly what failed.
// Returns false when there is no verify signal yet.
func verifyPin(v VerifyResult) (llm.Message, bool) {
	if v.Command == "" {
		return llm.Message{}, false
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Last verify: %s\npassed=%t exit=%d", v.Command, v.Passed, v.ExitCode)
	if !v.Passed {
		if out := strings.TrimSpace(v.Stdout + "\n" + v.Stderr); out != "" {
			b.WriteString("\n")
			b.WriteString(out)
		}
	}
	return llm.Message{Role: llm.RoleUser, Content: b.String()}, true
}

// filePin synthesizes the current-file pin from the FRESH on-disk content
// (re-read this turn through the jail), never the stale transcript copy. Returns
// false when no file is under edit. The body is re-read next turn, so truncating
// it under budget pressure is recoverable (§2.4 shed order, step 4).
func filePin(path, fresh string) (llm.Message, bool) {
	if path == "" || fresh == "" {
		return llm.Message{}, false
	}
	return llm.Message{
		Role:    llm.RoleUser,
		Content: "Current file under edit (re-read fresh from disk): " + path + "\n" + fresh,
	}, true
}

// recentTail returns the transcript after the task message, dropping any
// observation that is a stale read-dump of editPath — that file is re-read fresh
// into the file pin, so an old dump of it is wasted, possibly-stale tokens.
func recentTail(convo []llm.Message, editPath string) []llm.Message {
	if len(convo) <= 1 {
		return nil
	}
	stale := staleReadDumps(convo, editPath)
	out := make([]llm.Message, 0, len(convo)-1)
	for i := 1; i < len(convo); i++ {
		if stale[i] {
			continue
		}
		out = append(out, convo[i])
	}
	return out
}

// staleReadDumps marks the indices of observation messages that carry a read_file
// result for editPath: the observation immediately follows the assistant message
// whose tool call read that path. Empty editPath ⇒ nothing stale.
func staleReadDumps(convo []llm.Message, editPath string) map[int]bool {
	stale := map[int]bool{}
	if editPath == "" {
		return stale
	}
	for i, m := range convo {
		if m.Role != llm.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == tools.NameReadFile && argString(tc.Function.Arguments, "path") == editPath {
				if i+1 < len(convo) {
					stale[i+1] = true // the observation carrying the dump
				}
			}
		}
	}
	return stale
}

// argString extracts a string field from a tool call's JSON arguments ("" when
// absent or unparsable).
func argString(arguments, key string) string {
	if arguments == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(arguments), &m); err != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// keepTailWithin keeps the newest tail messages whose cumulative tokens fit
// budget (most recent nearest the end). It returns the kept slice and whether
// any message was dropped to fit.
func keepTailWithin(tail []llm.Message, budget int) ([]llm.Message, bool) {
	if budget <= 0 {
		return nil, len(tail) > 0
	}
	used, start := 0, len(tail)
	for i := len(tail) - 1; i >= 0; i-- {
		t := repomap.ApproxTokens(tail[i].Content)
		if used+t > budget {
			break
		}
		used += t
		start = i
	}
	return tail[start:], start > 0
}

// summarizeCold turns the cold middle into running-summary entries by structural
// extraction (overview §3: keep diffs + verify/exit outcomes verbatim; drop raw
// file dumps and stale prose) — NO LLM call. Each kept item is bounded by
// perItemCap (a larger one is truncated head/tail-with-marker, never dropped).
func summarizeCold(cold []llm.Message, perItemCap int) []string {
	entries := []string{}
	lastReadPath := ""
	for _, m := range cold {
		if m.Role == llm.RoleAssistant {
			if len(m.ToolCalls) == 0 {
				continue // stale assistant prose: dropped
			}
			for _, tc := range m.ToolCalls {
				switch tc.Function.Name {
				case tools.NameEditFile, tools.NameWriteFile:
					// Applied diff/content — kept verbatim (the actionable record).
					entries = append(entries, capEntry(editSigFromArgs(tc.Function.Name, tc.Function.Arguments), perItemCap))
				case tools.NameRunCommand:
					if cmd := argString(tc.Function.Arguments, "command"); cmd != "" {
						entries = append(entries, capEntry("$ "+cmd, perItemCap))
					}
				case tools.NameReadFile:
					lastReadPath = argString(tc.Function.Arguments, "path") // the dump follows
				}
			}
			continue
		}
		// User observation.
		if isReadDump(m.Content) {
			// Raw file-read dump → one-line stub; the file is re-read from disk on
			// demand, never stored as truth (overview §3/§8).
			entries = append(entries, readStub(lastReadPath, m.Content))
			lastReadPath = ""
			continue
		}
		// Other results — run_command exit lines, verify-fail tails, small tool
		// outputs — kept verbatim up to the cap.
		if s := strings.TrimSpace(m.Content); s != "" {
			entries = append(entries, capEntry(s, perItemCap))
		}
	}
	return entries
}

// editSigFromArgs renders an applied edit's signature from a tool call's JSON
// args (path + diff/content), mirroring loop.go's editSignature for the summary.
func editSigFromArgs(name, arguments string) string {
	path := argString(arguments, "path")
	switch name {
	case tools.NameEditFile:
		return "edit_file " + path + "\n" + argString(arguments, "diff")
	case tools.NameWriteFile:
		return "write_file " + path + "\n" + argString(arguments, "content")
	default:
		return name + " " + path
	}
}

// isReadDump reports whether content is a read_file observation (produced by the
// loop's observation() renderer), so it can be stubbed instead of stored.
func isReadDump(content string) bool {
	return strings.HasPrefix(content, "tool "+tools.NameReadFile+" result:")
}

// readStub is the one-line replacement for a file-read dump.
func readStub(path, content string) string {
	if path == "" {
		path = "(unknown)"
	}
	lines := strings.Count(content, "\n") + 1
	return fmt.Sprintf("[read %s: %d lines, re-read on demand]", path, lines)
}

// capEntry bounds a summary entry at capTok tokens, truncating head/tail-with-
// marker when it is larger (never dropping it).
func capEntry(s string, capTok int) string {
	t, _ := truncateToTokens(s, capTok)
	if t == "" {
		return s // cap too small to truncate meaningfully; the shed phase bounds the whole
	}
	return t
}

// truncateToTokens shortens s to ≤ capTok tokens, preserving the HEAD and TAIL
// and eliding the middle with an explicit marker (so a diff/verify tail's
// actionable ends survive). It returns (result, truncated). capTok ≤ 0 fully
// elides (returns ""). A string already within the cap is returned unchanged.
func truncateToTokens(s string, capTok int) (string, bool) {
	if capTok <= 0 {
		return "", true
	}
	if repomap.ApproxTokens(s) <= capTok {
		return s, false
	}
	elided := repomap.ApproxTokens(s) - capTok
	marker := fmt.Sprintf("\n… [truncated ~%d tokens; re-run verify / re-read on demand] …\n", elided)
	bodyTok := capTok - repomap.ApproxTokens(marker)
	if bodyTok < 2 {
		if repomap.ApproxTokens(marker) <= capTok {
			return marker, true // no room for head+tail; a bare marker still fits
		}
		return "", true
	}
	bodyChars := bodyTok * 4
	if bodyChars >= len(s) {
		return s, false
	}
	head, tail := bodyChars/2, bodyChars-bodyChars/2
	out := s[:head] + marker + s[len(s)-tail:]
	for repomap.ApproxTokens(out) > capTok && head > 0 {
		head -= 8
		tail -= 8
		if head < 0 {
			head = 0
		}
		if tail < 0 {
			tail = 0
		}
		out = s[:head] + marker + s[len(s)-tail:]
	}
	return out, true
}
