package tui

import "github.com/charmbracelet/lipgloss"

// theme.go is the single home of the TUI's colour vocabulary. Every colour lives
// here as a named, profile-aware lipgloss style; no other file in internal/tui
// constructs a lipgloss.Color literal (a Go test in theme_test.go guards this).
//
// The colour codes match the literals these styles replaced (212/2/1/3/244) so
// the Phase 01 migration is visually a no-op — Phase 02 owns any retune. Because
// lipgloss resolves a Color through the active termenv profile, NO_COLOR /
// non-TTY degrade falls out automatically once the entrypoint forces the ascii
// profile (task 03).

// Semantic colour codes — the one place a colour value is written. Exposed
// (package-private) so border tints can pull the same colour as the matching
// text style (e.g. a danger border and danger text share dangerColor).
var (
	accentColor  = lipgloss.Color("212") // pink/magenta brand accent
	successColor = lipgloss.Color("2")   // green
	dangerColor  = lipgloss.Color("1")   // red
	warningColor = lipgloss.Color("3")   // yellow/amber
	mutedColor   = lipgloss.Color("244") // dim grey
)

// Semantic styles — the named roles callers reference instead of raw colours.
// Package-private: every caller is in package tui (naming.md keeps the internal
// surface minimal).
var (
	accent  = lipgloss.NewStyle().Foreground(accentColor)
	success = lipgloss.NewStyle().Foreground(successColor)
	danger  = lipgloss.NewStyle().Foreground(dangerColor)
	warning = lipgloss.NewStyle().Foreground(warningColor)
	muted   = lipgloss.NewStyle().Foreground(mutedColor)
)

// toolAccent maps a tool name to its card accent + glyph (Phase 02 consumes
// these; this file only defines them). Colours resolve through the active
// termenv profile, so NO_COLOR / non-TTY degrade automatically.
type toolAccent struct {
	style lipgloss.Style
	glyph string
}

// toolAccents is keyed by the agent's snake_case tool names (naming.md §Tool
// names) so a lookup is direct from a toolEventMsg.Name.
var toolAccents = map[string]toolAccent{
	"run_command": {style: accent, glyph: "⌘"},
	"edit_file":   {style: success, glyph: "✎"},
	"read_file":   {style: muted, glyph: "👁"},
	// verify-flavoured card (Phase 02 task 01): green ✓ identity, matching the
	// pass/fail semantics used elsewhere.
	"verify": {style: success, glyph: glyphPass},
}

// verify glyphs (pass/fail) — green ✓ / red ✗, reused by run cards + reports.
var (
	verifyPass = success // renders "✓"
	verifyFail = danger  // renders "✗"
)

const (
	glyphPass = "✓"
	glyphFail = "✗"
)

// accentFor returns the card accent+glyph for a tool, defaulting to the muted
// style + a bullet for tools without a dedicated accent.
func accentFor(name string) toolAccent {
	if a, ok := toolAccents[name]; ok {
		return a
	}
	return toolAccent{style: muted, glyph: "•"}
}

// toolChip renders a card header chip — the tool's leading glyph + label in the
// tool's accent (Phase 02 cards). Secondary text (paths/commands/summaries) is
// rendered separately in `muted` by the caller.
func toolChip(name string) string {
	a := accentFor(name)
	return a.style.Render(a.glyph + " " + name)
}

// ── Phase 02 surface styles — all built from the existing 5-colour palette plus
// text attributes (bold), so no new colour enters the vocabulary. ──────────────

// sep is the dim rule between diff hunks (and any structural separator).
var sep = muted

// expandHint styles the "… +K more lines  ctrl+o to expand" affordance as calm
// secondary text (consistent with the dimmed command body it follows).
var expandHint = muted

// Markdown-ish assistant styles (task 04). Under the ascii test profile these
// strip to plain text — so goldens are deterministic and markers are simply
// removed — while a real terminal gets the hierarchy.
var (
	mdHeading = accent.Bold(true)                            // # headers
	mdBold    = lipgloss.NewStyle().Bold(true)               // **bold**
	mdCode    = lipgloss.NewStyle().Foreground(successColor) // `inline code`
	mdBullet  = accent                                       // • bullet glyph
)
