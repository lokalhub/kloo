package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// item is one rendered block in the scrolling transcript.
type item interface {
	render(width int) string
}

// userItem is a submitted task line ("▸ you: …").
type userItem struct{ text string }

func (u userItem) render(width int) string {
	return wrapLines("▸ you: "+u.text, width)
}

// assistantItem is the assistant's (possibly still-streaming) reply.
type assistantItem struct {
	content   string
	streaming bool
}

func (a assistantItem) render(width int) string {
	head := "● assistant"
	if a.streaming {
		head += " (streaming…)"
	}
	// Strip raw tool-call syntax (JSON / <tool_call> / <function=…>) that
	// no-native-FC models emit inline — complete blocks are removed and a
	// still-streaming partial call is truncated at its opener, so only the
	// reasoning prose shows (the action is shown via the card + activity line).
	body := strings.TrimRight(cleanAssistantText(a.content), "\n")
	if body == "" {
		return head
	}
	// Wrap on plain text first, then apply the light markdown styler per visual
	// line (task 04): headers/bold/code/bullets gain hierarchy; plain prose is
	// unchanged. Styling-after-wrap keeps real-terminal ANSI escapes intact.
	styled := stylizeMarkdown(wrapLines(body, width-2))
	return head + "\n" + indent(styled, "  ")
}

// infoItem is a one-line notice (slash echoes, unknown command, errors).
type infoItem struct{ text string }

func (i infoItem) render(width int) string { return wrapLines(i.text, width) }

// interruptItem marks a user-initiated interrupt (distinct from the autonomous
// stop-report banner).
type interruptItem struct{}

func (interruptItem) render(width int) string {
	return cardStyle(width, lipgloss.NormalBorder()).Render("■ run interrupted — returned to idle (partial state preserved)")
}

// --- card rendering helpers -------------------------------------------------

// cardStyle is the shared bordered-card style at the given width.
func cardStyle(width int, border lipgloss.Border) lipgloss.Style {
	w := width - 2
	if w < 10 {
		w = 10
	}
	return lipgloss.NewStyle().Border(border).Width(w).Padding(0, 1)
}

// wrapLines hard-wraps text to width, preserving existing newlines.
func wrapLines(s string, width int) string {
	if width < 4 {
		width = 4
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		for len([]rune(line)) > width {
			r := []rune(line)
			out = append(out, string(r[:width]))
			line = string(r[width:])
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// indent prefixes every line with pad.
func indent(s, pad string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = pad + lines[i]
	}
	return strings.Join(lines, "\n")
}

// truncate shortens s to n runes with an ellipsis.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 1 {
		return ""
	}
	return string(r[:n-1]) + "…"
}

func human(n int) string {
	if n >= 1000 {
		if n%1000 == 0 {
			return fmt.Sprintf("%dk", n/1000)
		}
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%d", n)
}
