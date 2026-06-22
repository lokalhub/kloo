package tui

import "testing"

func TestOSC52Seq(t *testing.T) {
	// base64("hi") == "aGk="; the escape is ESC ] 52 ; c ; <b64> BEL.
	if got, want := osc52Seq("hi"), "\x1b]52;c;aGk=\a"; got != want {
		t.Errorf("osc52Seq = %q, want %q", got, want)
	}
}

func TestLastAssistantText(t *testing.T) {
	m := newSized()
	if got := m.lastAssistantText(); got != "" {
		t.Errorf("empty transcript should yield %q, got %q", "", got)
	}
	// Last assistant reply is returned, with tool-call JSON stripped (as displayed).
	m.transcript = append(m.transcript,
		userItem{text: "hi"},
		assistantItem{content: "first reply"},
		assistantItem{content: `second reply {"name":"read","arguments":{"path":"x"}}`},
	)
	if got := m.lastAssistantText(); got != "second reply" {
		t.Errorf("lastAssistantText = %q, want %q", got, "second reply")
	}
}
