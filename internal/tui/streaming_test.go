package tui

import "testing"

// TestStreamingTokenByToken: deltas append live; intermediate frames show the
// partial text; the final frame drops the "streaming…" marker and equals the
// accumulated content.
func TestStreamingTokenByToken(t *testing.T) {
	m := sized(New(Config{Model: "snappy", MaxSteps: 40, MaxTokens: 8000}), tw, th)

	// First delta starts the streaming block.
	m = apply(m, streamDeltaMsg{Content: "I'll "})
	if got := m.streamingText(); got != "I'll " {
		t.Fatalf("after 1 delta, streamingText = %q", got)
	}
	// More deltas accumulate.
	m = apply(m, streamDeltaMsg{Content: "update "}, streamDeltaMsg{Content: "the routes."})
	if got := m.streamingText(); got != "I'll update the routes." {
		t.Fatalf("accumulated = %q", got)
	}
	// While streaming, the marker is present.
	if !contains(m.View(), "(streaming…)") {
		t.Errorf("streaming marker should be visible mid-stream")
	}

	// Completion drops the marker; final text == accumulated deltas.
	m = apply(m, streamDoneMsg{Content: "I'll update the routes."})
	if m.streamIdx != -1 {
		t.Errorf("stream should be finalized (streamIdx=-1), got %d", m.streamIdx)
	}
	if contains(m.View(), "(streaming…)") {
		t.Errorf("streaming marker should be gone after done")
	}
	// The finalized assistant text equals the concatenated deltas.
	final := m.transcript[len(m.transcript)-1].(assistantItem)
	if final.streaming || final.content != "I'll update the routes." {
		t.Errorf("finalized message = %+v", final)
	}
	requireGolden(t, "streaming-final.golden", m.View())
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
