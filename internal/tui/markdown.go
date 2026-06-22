package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// markdown.go is a hand-rolled *light* markdown styler for assistant prose
// (Phase 02 task 04) — NOT glamour/CommonMark. It handles `#`/`##`/`###`
// headers, `-`/`*` bullets, inline `**bold**`, and inline `` `code` ``, using the
// theme styles. Under the ascii test profile the styles strip to plain text, so
// markers are simply removed/replaced and goldens stay deterministic; a real
// terminal gets the hierarchy. Everything else passes through unchanged, and an
// unterminated inline marker (common mid-stream) degrades to literal text.

// stylizeMarkdown applies the light styler line-by-line to already-wrapped plain
// text. Wrapping must happen first (on plain text) so a real terminal's ANSI
// escapes are never split mid-sequence by the wrapper.
func stylizeMarkdown(wrapped string) string {
	lines := strings.Split(wrapped, "\n")
	for i, line := range lines {
		lines[i] = styleMarkdownLine(line)
	}
	return strings.Join(lines, "\n")
}

// styleMarkdownLine classifies a single line (header / bullet / normal) — block
// markers are line-anchored — then applies inline styling to its text.
func styleMarkdownLine(line string) string {
	if text, ok := headerText(line); ok {
		return mdHeading.Render(applyInline(text))
	}
	if text, ok := bulletText(line); ok {
		return mdBullet.Render("• ") + applyInline(text)
	}
	return applyInline(line)
}

// headerText returns the text of a `#`/`##`/`###` heading line (marker stripped),
// or ok=false for a non-heading.
func headerText(line string) (string, bool) {
	n := 0
	for n < len(line) && line[n] == '#' {
		n++
	}
	if n >= 1 && n <= 3 && n < len(line) && line[n] == ' ' {
		return strings.TrimSpace(line[n+1:]), true
	}
	return "", false
}

// bulletText returns the item text of a `- ` / `* ` bullet line, or ok=false.
func bulletText(line string) (string, bool) {
	if strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ") {
		return line[2:], true
	}
	return "", false
}

// applyInline styles inline `**bold**` then inline “ `code` “. Unterminated
// markers are left literal (streaming-safe).
func applyInline(s string) string {
	s = replacePaired(s, "**", mdBold)
	s = replacePaired(s, "`", mdCode)
	return s
}

// replacePaired replaces each matched pair of delim with style.Render(inner). An
// unmatched (unterminated) delim and everything after it is emitted literally, so
// a partial stream never produces a dangling style or a panic.
func replacePaired(s, delim string, style lipgloss.Style) string {
	var b strings.Builder
	for {
		i := strings.Index(s, delim)
		if i < 0 {
			b.WriteString(s)
			break
		}
		rest := s[i+len(delim):]
		j := strings.Index(rest, delim)
		if j < 0 {
			b.WriteString(s) // unterminated — literal
			break
		}
		b.WriteString(s[:i])
		b.WriteString(style.Render(rest[:j]))
		s = rest[j+len(delim):]
	}
	return b.String()
}
