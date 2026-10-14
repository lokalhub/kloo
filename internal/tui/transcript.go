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
// stop-report banner). dropped means the user pressed Esc a SECOND time, before
// the loop had finished unwinding: the prompt came back immediately and the run
// was abandoned rather than waited for.
type interruptItem struct{ dropped bool }

func (i interruptItem) render(width int) string {
	text := "■ run interrupted — returned to idle (partial state preserved)"
	if i.dropped {
		text = "■ run dropped — back to the prompt; it was still stopping, so its late output is ignored (partial state preserved)"
	}
	return cardStyle(width, lipgloss.NormalBorder()).Render(text)
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

// wrapLines wraps text to width at WORD boundaries, preserving existing newlines.
//
// It used to cut at exactly `width` runes wherever that landed, so every answer
// long enough to wrap came out with a word sawn in half ("a more specific in" /
// "struction"). Nothing else in the terminal does that, and it is a large part of
// why kloo's prose did not read like Claude's or grok's on the same reply.
//
// A single word longer than the line (a path, a URL, a hash) is still hard-split —
// there is nowhere else to break it — and no character is ever dropped.
func wrapLines(s string, width int) string {
	if width < 4 {
		width = 4
	}
	var out []string
	for _, line := range strings.Split(s, "\n") {
		// Leading whitespace is structure (indented code, list continuation); keep it
		// on the first visual line and measure the rest against the same width.
		cur := ""
		flush := func() {
			out = append(out, cur)
			cur = ""
		}
		for _, word := range splitKeepingSpaces(line) {
			switch {
			case cur == "":
				cur = word
			case len([]rune(cur))+1+len([]rune(word)) <= width:
				cur += " " + word
			default:
				flush()
				cur = word
			}
			// A word that cannot fit a line of its own: hard-split the overflow.
			for len([]rune(cur)) > width {
				r := []rune(cur)
				out = append(out, string(r[:width]))
				cur = string(r[width:])
			}
		}
		flush()
	}
	return strings.Join(out, "\n")
}

// splitKeepingSpaces splits a line into words, keeping any leading indentation
// attached to the first word so wrapped code/list lines do not lose their shape.
func splitKeepingSpaces(line string) []string {
	trimmed := strings.TrimLeft(line, " \t")
	indent := line[:len(line)-len(trimmed)]
	words := strings.Fields(trimmed)
	if len(words) == 0 {
		return []string{indent}
	}
	words[0] = indent + words[0]
	return words
}

// hangingNote renders a "■ <sentence>" line whose wrapped continuation lines are
// indented under the text, not under the marker — so one note reads as one note.
func hangingNote(marker, text string, width int) string {
	w := width - lipgloss.Width(marker)
	if w < 8 {
		w = 8
	}
	wrapped := strings.Split(wrapLines(text, w), "\n")
	pad := strings.Repeat(" ", lipgloss.Width(marker))
	for i := range wrapped {
		if i == 0 {
			continue
		}
		wrapped[i] = pad + wrapped[i]
	}
	return marker + strings.Join(wrapped, "\n")
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
