package tui

import tea "github.com/charmbracelet/bubbletea"

// streamDeltaMsg is one streamed assistant content delta (from the P00 Stream
// callback, bridged into the program via tea.Program.Send).
type streamDeltaMsg struct{ Content string }

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
		nt := appendItems(m.transcript[:m.streamIdx:m.streamIdx], cur)
		nt = append(nt, m.transcript[m.streamIdx+1:]...)
		m.transcript = nt
		m.streamIdx = -1
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
		m.vp.SetContent(m.renderTranscript())
		m.vp.GotoBottom()
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
