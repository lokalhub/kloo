package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// stripToolMarkup removes a trailing tool-call markup block from displayed
// assistant prose. Some models (e.g. DeepSeek's <｜DSML｜invoke…｜> dialect, or the
// <function=…> form) emit the tool call as text after their prose; kloo PARSES and
// executes it (native_fc text fallbacks), but the raw markup would otherwise show
// in the transcript. Cut at the earliest such opener — these tokens never occur in
// real prose. Display-only; the parse path is unaffected.
func stripToolMarkup(s string) string {
	cut := -1
	for _, mk := range []string{"<｜", "<function=", "<tool_call>"} {
		if i := strings.Index(s, mk); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut < 0 {
		return s
	}
	return strings.TrimRight(s[:cut], " \t\r\n")
}

// streamDeltaMsg is one streamed assistant content delta (from the P00 Stream
// callback, bridged into the program via tea.Program.Send).
type streamDeltaMsg struct{ Content string }

// noticeMsg is a transient one-line status note (e.g. a model-call retry) shown as
// a dim info line, so a flaky endpoint that's being retried isn't a silent stall.
type noticeMsg struct{ text string }

// streamDoneMsg marks the end of a stream; Content is the fully accumulated
// message (which, by construction, equals the concatenated deltas).
type streamDoneMsg struct {
	Content string
	Err     error
}

// handleStreamDelta appends a delta to the in-progress assistant message,
// starting one if none is active, and auto-scrolls to the tail.
func (m Model) handleStreamDelta(msg streamDeltaMsg) (tea.Model, tea.Cmd) {
	if m.streamIdx < 0 {
		m.transcript = appendItems(m.transcript, assistantItem{content: msg.Content, streaming: true})
		m.streamIdx = len(m.transcript) - 1
	} else {
		cur := m.transcript[m.streamIdx].(assistantItem)
		cur.content += msg.Content
		nt := appendItems(m.transcript[:m.streamIdx:m.streamIdx], cur)
		nt = append(nt, m.transcript[m.streamIdx+1:]...)
		m.transcript = nt
	}
	return m.refreshViewport(), nil
}

// handleStreamDone finalizes the streaming message (drops the "streaming…"
// marker). The displayed text stays the delta-accumulated content, so the
// finalized message equals the accumulated deltas.
func (m Model) handleStreamDone(msg streamDoneMsg) (tea.Model, tea.Cmd) {
	if msg.Err != nil {
		m.streamIdx = -1
		return m.appendItem(infoItem{text: "stream error: " + msg.Err.Error()}), nil
	}
	if m.streamIdx >= 0 {
		cur := m.transcript[m.streamIdx].(assistantItem)
		cur.streaming = false
		cur.content = stripToolMarkup(cur.content) // hide any trailing tool-call markup the model emitted as text
		nt := appendItems(m.transcript[:m.streamIdx:m.streamIdx], cur)
		nt = append(nt, m.transcript[m.streamIdx+1:]...)
		m.transcript = nt
		m.streamIdx = -1
		// C8: pin the latest finalized assistant prose above the input while running.
		if s := summaryLine(cur.content); s != "" {
			m.latestSummary = s
		}
	}
	return m.refreshViewport(), nil
}

// appendItem appends an item, copying the slice (no aliasing) and refreshing the
// viewport.
func (m Model) appendItem(it item) Model {
	m.transcript = appendItems(m.transcript, it)
	return m.refreshViewport()
}

// appendItems returns a fresh slice with the items appended (copy-on-write).
func appendItems(base []item, more ...item) []item {
	out := make([]item, 0, len(base)+len(more))
	out = append(out, base...)
	out = append(out, more...)
	return out
}

// refreshViewport re-renders the transcript into the viewport and scrolls to the
// streaming tail.
func (m Model) refreshViewport() Model {
	if m.vpReady {
		// Sticky bottom: auto-scroll to the tail ONLY if the user was already at the
		// bottom. If they scrolled up to read earlier output, new content must not
		// yank them back down (that was the "can't scroll back" bug).
		atBottom := m.vp.AtBottom()
		m.vp.SetContent(m.transcriptContent())
		if atBottom {
			m.vp.GotoBottom()
		}
	}
	return m
}

// streamingText returns the in-progress assistant content (test helper / state).
func (m Model) streamingText() string {
	if m.streamIdx < 0 {
		return ""
	}
	if a, ok := m.transcript[m.streamIdx].(assistantItem); ok {
		return a.content
	}
	return ""
}
