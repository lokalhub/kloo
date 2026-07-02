package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// editPair is one parsed SEARCH/REPLACE block, rendered as `-` (search) / `+`
// (replace) diff lines. An empty search is the new-file form (all `+` lines).
type editPair struct {
	search, replace string
}

// toolEventMsg carries a loop tool call to render as a card. For edit_file it
// carries the parsed SEARCH/REPLACE blocks (Edits); for run_command the command
// + captured exit code and stderr; other tools carry a one-line summary.
type toolEventMsg struct {
	Name string
	Path string // edit_file / write_file target
	// Edits holds the parsed edit blocks (the real path — the bridge fills this
	// from edit.Parse). For a single pre-split pair, tests may instead set
	// Search/Replace, which handleToolEvent folds into one editPair.
	Edits    []editPair
	Search   string // convenience: a single SEARCH (tests)
	Replace  string // convenience: a single REPLACE (tests)
	Command  string // run_command command
	ExitCode int    // run_command exit code
	Stderr   string // run_command captured stderr (failure body)
	Summary  string // generic tools (read_file/list_dir/write_file)
}

// editsOf resolves the edit pairs from a tool event (Edits, else the single
// Search/Replace convenience pair).
func editsOf(msg toolEventMsg) []editPair {
	if len(msg.Edits) > 0 {
		return msg.Edits
	}
	if msg.Search != "" || msg.Replace != "" {
		return []editPair{{search: msg.Search, replace: msg.Replace}}
	}
	return nil
}

// handleToolEvent appends the appropriate card to the transcript. It also sets
// the display-only activity phrase (task 06) from the tool, so the thinking line
// can show "editing <file>" / "running <cmd>"; this is TUI-side only (no loop
// signal, no internal/agent change).
func (m Model) handleToolEvent(msg toolEventMsg) (tea.Model, tea.Cmd) {
	m.activity = activityPhrase(msg)
	// C8: also fold the completed tool into the compact active-run log.
	m = m.pushActivity(activityFromTool(msg))
	var it item
	switch msg.Name {
	case "edit_file":
		it = editCardItem{path: msg.Path, edits: editsOf(msg)}
	case "run_command":
		it = runCardItem{command: msg.Command, exitCode: msg.ExitCode, stderr: msg.Stderr}
	default:
		summary := msg.Summary
		if summary == "" {
			summary = msg.Path
		}
		it = genericCardItem{name: msg.Name, summary: summary}
	}
	return m.appendItem(it), nil
}

// commandVerb maps a shell command to a friendly activity verb so the activity
// line reads "moving <a> <b>" / "removing <x>" rather than "running mv …".
func commandVerb(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return "running"
	}
	rest := strings.TrimSpace(strings.TrimPrefix(cmd, fields[0]))
	switch fields[0] {
	case "mv":
		return "moving " + rest
	case "cp":
		return "copying " + rest
	case "rm":
		return "removing " + rest
	case "mkdir":
		return "creating " + rest
	case "touch":
		return "creating " + rest
	default:
		return "running " + cmd
	}
}

// activityPhrase derives the compact in-flight activity phrase from a tool event
// (display-only). Empty tools fall back to the tool name.
func activityPhrase(msg toolEventMsg) string {
	switch msg.Name {
	case "edit_file", "write_file":
		if msg.Path != "" {
			return "editing " + msg.Path
		}
		return "editing"
	case "run_command":
		if msg.Command != "" {
			return commandVerb(msg.Command)
		}
		return "running"
	case "read_file":
		if s := msg.Summary; s != "" {
			return "reading " + s
		}
		if msg.Path != "" {
			return "reading " + msg.Path
		}
		return "reading"
	case "read_dir":
		if msg.Path != "" {
			return "reading folder " + msg.Path
		}
		return "reading folder"
	case "search":
		return "searching"
	default:
		return msg.Name
	}
}

// editCardItem renders an edit_file as a -/+ diff card: each parsed block's
// SEARCH lines as `-` and REPLACE lines as `+` (an empty SEARCH = new file, all
// `+`). The raw fence/markers are never shown — only the diff.
type editCardItem struct {
	path  string
	edits []editPair
}

func (e editCardItem) render(width int) string {
	ea := accentFor("edit_file")
	// ✎ <path> header line in the edit_file accent (diff-card.html). The whole
	// header is the card's identity, so it carries the accent (not dimmed).
	lines := []string{ea.style.Render(ea.glyph + " " + e.path)}
	for i, p := range e.edits {
		if i > 0 {
			// Dim separator between consecutive hunks; none before the first.
			lines = append(lines, sep.Render(strings.Repeat("─", diffSepWidth(width))))
		}
		lines = append(lines, hunkLines(p)...)
	}
	return cardStyle(width, lipgloss.NormalBorder()).Render(strings.Join(lines, "\n"))
}

// hunkLines renders one edit pair as a MINIMAL line diff between SEARCH and
// REPLACE: unchanged anchor/context lines as dim context, only genuinely changed
// lines as red `- ` / green `+ ` (an empty SEARCH is the new-file form — all `+`).
// Dumping the whole SEARCH as `-` and whole REPLACE as `+` duplicated every
// context line as a remove+add pair (noisy); this shows just the change. The apply
// path still matches the exact SEARCH text — this is display-only.
func hunkLines(p editPair) []string {
	return lineDiff(splitDiffLines(p.search), splitDiffLines(p.replace))
}

// splitDiffLines splits a SEARCH/REPLACE body into lines (trailing newline
// trimmed); "" ⇒ no lines (e.g. a new-file edit's empty SEARCH).
func splitDiffLines(s string) []string {
	if s = strings.TrimRight(s, "\n"); s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// lineDiff renders a minimal line-level diff (LCS) of old→new: unchanged lines as
// dim context, removed lines as `- `, added lines as `+ `. Deterministic, pure Go,
// display-only. Empty old (new file) ⇒ all `+`; empty new ⇒ all `-`.
func lineDiff(oldL, newL []string) []string {
	n, m := len(oldL), len(newL)
	c := make([][]int, n+1) // LCS length table, filled back-to-front
	for i := range c {
		c[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldL[i] == newL[j] {
				c[i][j] = c[i+1][j+1] + 1
			} else if c[i+1][j] >= c[i][j+1] {
				c[i][j] = c[i+1][j]
			} else {
				c[i][j] = c[i][j+1]
			}
		}
	}
	var out []string
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case oldL[i] == newL[j]:
			out = append(out, muted.Render("  "+oldL[i]))
			i, j = i+1, j+1
		case c[i+1][j] >= c[i][j+1]:
			out = append(out, diffMinus.Render("- "+oldL[i]))
			i++
		default:
			out = append(out, diffPlus.Render("+ "+newL[j]))
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, diffMinus.Render("- "+oldL[i]))
	}
	for ; j < m; j++ {
		out = append(out, diffPlus.Render("+ "+newL[j]))
	}
	return out
}

// diffSepWidth sizes the inter-hunk rule to roughly the card's inner content
// width (border + padding subtracted), clamped so it never overflows or vanishes.
func diffSepWidth(width int) int {
	n := width - 6
	if n < 8 {
		n = 8
	}
	if n > 60 {
		n = 60
	}
	return n
}

// runCardItem renders a run_command as a chip header + coloured exit result and,
// on failure, a few dim stderr lines. Long output is truncated with a
// `ctrl+o to expand` affordance unless `expanded` is set (stamped by
// renderTranscript from the model-level toggle, task 03).
type runCardItem struct {
	command  string
	exitCode int
	stderr   string
	expanded bool
}

// runBodyCap bounds the stderr lines shown before truncation (task 03).
const runBodyCap = 4

func (r runCardItem) render(width int) string {
	ok := r.exitCode == 0
	border := lipgloss.NormalBorder()
	style := cardStyle(width, border)

	// Coloured exit result (task 05): green pass / red fail; keep the failure
	// border tint for at-a-glance scannability.
	resultStyle := verifyPass
	glyph := glyphPass
	if !ok {
		resultStyle = verifyFail
		glyph = glyphFail
		style = style.BorderForeground(dangerColor)
	}
	marker := resultStyle.Render(fmt.Sprintf("exit %d %s", r.exitCode, glyph))

	// Chip header (task 01): ⌘ run_command accent + dim command + result.
	head := toolChip("run_command") + "  " + muted.Render(r.command) + "    " + marker

	lines := []string{head}
	if !ok {
		// Dim stderr body (task 03), truncated unless expanded.
		var body []string
		for _, line := range strings.Split(strings.TrimRight(r.stderr, "\n"), "\n") {
			if line != "" {
				body = append(body, line)
			}
		}
		shown, hidden := body, 0
		if !r.expanded && len(body) > runBodyCap {
			shown = body[:runBodyCap]
			hidden = len(body) - runBodyCap
		}
		for _, line := range shown {
			lines = append(lines, muted.Render("  "+line))
		}
		if hidden > 0 {
			lines = append(lines, expandHint.Render(fmt.Sprintf("  … +%d more lines  ctrl+o to expand", hidden)))
		}
	}
	return style.Render(strings.Join(lines, "\n"))
}

// genericCardItem renders a compact one-line card for non-edit/non-run tools: a
// per-tool chip (glyph + name in the tool accent) + dim secondary summary.
type genericCardItem struct {
	name    string
	summary string
}

func (g genericCardItem) render(width int) string {
	line := toolChip(g.name)
	if s := strings.TrimSpace(g.summary); s != "" {
		line += "  " + muted.Render(s)
	}
	// Read-only inspection tools (read_file/list_dir) are frequent and low-signal —
	// render them as a plain indented line, NOT a bordered card, to cut visual
	// noise (the one-line summary is enough). Mutations (write_file) keep the card
	// box for emphasis.
	switch g.name {
	case "read_file", "list_dir", "read_dir", "search":
		return "  " + line
	}
	return cardStyle(width, lipgloss.NormalBorder()).Render(line)
}

// Diff line styles source their colour from the central palette (theme.go):
// removed lines use danger (red), added lines use success (green). Colour is
// stripped under the non-TTY/ascii profile, so goldens compare the -/+ prefixes,
// which carry the intent.
var (
	diffMinus = danger
	diffPlus  = success
)
