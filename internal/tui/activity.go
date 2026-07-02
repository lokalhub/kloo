package tui

import (
	"strconv"
	"strings"
)

// activityKind selects the glyph/colour of a compact activity-log entry (C8). It
// mirrors the mock's ✓ done (green) / → running (accent) / ✗ fail (red) / dim info.
type activityKind int

const (
	actDone activityKind = iota // completed step (✓, green)
	actRun                      // in-progress / launched (→, accent)
	actFail                     // failed step (✗, red)
	actInfo                     // neutral note, e.g. a model-call retry (dim)
)

// activityEntry is one line of the compact active-run log.
type activityEntry struct {
	kind activityKind
	text string
}

// maxActivityLog bounds how many entries are retained; the log renders only the
// most recent activityLogVisible of these. Kept small — this is a live glance, not
// the transcript (which still records everything).
const (
	maxActivityLog     = 12
	activityLogVisible = 4
)

// pushActivity appends an entry to the rolling activity log (copy-on-write, capped),
// returning the updated model. Consecutive exact-duplicate entries are collapsed so
// a repeated identical tool call doesn't spam the glance log.
func (m Model) pushActivity(kind activityKind, text string) Model {
	text = strings.TrimSpace(text)
	if text == "" {
		return m
	}
	if n := len(m.activityLog); n > 0 && m.activityLog[n-1].kind == kind && m.activityLog[n-1].text == text {
		return m
	}
	next := make([]activityEntry, 0, len(m.activityLog)+1)
	next = append(next, m.activityLog...)
	next = append(next, activityEntry{kind: kind, text: text})
	if len(next) > maxActivityLog {
		next = next[len(next)-maxActivityLog:]
	}
	m.activityLog = next
	return m
}

// glyph returns the leading marker for an entry's kind.
func (e activityEntry) glyph() string {
	switch e.kind {
	case actDone:
		return glyphPass // ✓
	case actRun:
		return "→"
	case actFail:
		return glyphFail // ✗
	default:
		return "·"
	}
}

// styled renders the entry as "<glyph> <text>" in its kind's colour.
func (e activityEntry) styled() string {
	line := e.glyph() + " " + e.text
	switch e.kind {
	case actDone:
		return success.Render(line)
	case actRun:
		return accent.Render(line)
	case actFail:
		return danger.Render(line)
	default:
		return muted.Render(line)
	}
}

// activityFromTool derives a compact log entry (kind + text) from a completed tool
// event — the same signal the transcript cards use, flattened to one glance line.
func activityFromTool(msg toolEventMsg) (activityKind, string) {
	switch msg.Name {
	case "edit_file", "write_file":
		verb := "edited"
		if msg.Name == "write_file" {
			verb = "wrote"
		}
		if msg.Path != "" {
			return actDone, verb + " " + msg.Path
		}
		return actDone, verb
	case "run_command":
		cmd := firstLine(msg.Command)
		if msg.ExitCode != 0 {
			return actFail, "ran " + cmd + " (exit " + strconv.Itoa(msg.ExitCode) + ")"
		}
		return actDone, "ran " + cmd
	case "read_file":
		return actDone, "read " + orDash(msg.Summary, msg.Path)
	case "list_dir":
		return actDone, "listed " + orDash(msg.Summary, msg.Path)
	case "read_dir":
		return actDone, "read " + orDash(msg.Summary, msg.Path)
	case "search":
		return actDone, "searched " + msg.Summary
	default:
		return actDone, orDash(msg.Summary, msg.Name)
	}
}

// orDash returns the first non-empty of the candidates, else "…".
func orDash(candidates ...string) string {
	for _, c := range candidates {
		if strings.TrimSpace(c) != "" {
			return c
		}
	}
	return "…"
}

// summaryLine condenses finalized assistant prose to one line for the pinned
// "Latest assistant" region: collapse whitespace/newlines and keep the leading
// sentence-ish chunk. Empty in → empty out (region stays hidden).
func summaryLine(content string) string {
	s := strings.Join(strings.Fields(content), " ")
	return s
}

// firstLine returns the first line of s (trimmed), for compact command display.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
