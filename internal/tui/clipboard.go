package tui

import (
	"encoding/base64"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// clipboardMsg reports that text was sent to the system clipboard (chars copied).
type clipboardMsg struct{ chars int }

// osc52Seq builds the OSC 52 "set clipboard" escape for text. OSC 52 lets a
// terminal app write the system clipboard with no external tool (xclip/pbcopy) and
// works over SSH — the terminal consumes the escape; it draws nothing.
func osc52Seq(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
}

// copyToClipboard returns a command that copies text to the system clipboard via
// OSC 52. It writes to stderr (not stdout) so it never races Bubble Tea's renderer,
// while still reaching the controlling terminal. Requires terminal OSC 52 support
// (kitty, iTerm2, WezTerm, Alacritty, recent VTE, tmux with set-clipboard on);
// where unsupported, Shift+drag native selection is the fallback.
func copyToClipboard(text string) tea.Cmd {
	return func() tea.Msg {
		_, _ = os.Stderr.WriteString(osc52Seq(text))
		return clipboardMsg{chars: len(text)}
	}
}

// lastAssistantText returns the most recent assistant reply as displayed (tool-call
// syntax stripped), or "" if there isn't one yet — what Ctrl+Y copies.
func (m Model) lastAssistantText() string {
	for i := len(m.transcript) - 1; i >= 0; i-- {
		if a, ok := m.transcript[i].(assistantItem); ok {
			return strings.TrimSpace(cleanAssistantText(a.content))
		}
	}
	return ""
}
