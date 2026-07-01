package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// slashCommand is one selectable entry in the inline slash-command menu. noArg
// marks a command that takes no argument, so Enter EXECUTES it immediately
// instead of inserting it for the user to type arguments after.
type slashCommand struct {
	name  string
	desc  string
	noArg bool
}

// slashCommands is the menu's catalogue, in display order. It mirrors runSlash's
// dispatch (commands.go) — keep the two in sync when commands are added/removed.
var slashCommands = []slashCommand{
	{name: "/model", desc: "switch the active model"},
	{name: "/models", desc: "list available models", noArg: true},
	{name: "/provider", desc: "switch provider (endpoint+key)"},
	{name: "/add", desc: "pin a file to context"},
	{name: "/mode", desc: "set permission dial"},
}

// slashMenu is the lightweight inline command menu shown above the input while
// the user is typing a command name (the line starts with "/" and has no space
// yet). It is recomputed from the input on every keystroke (syncSlashMenu); index
// is the highlighted row. Distinct from the full model picker overlay.
type slashMenu struct {
	items []slashCommand
	index int
}

// filterSlashCommands returns the commands whose name (sans leading "/") has the
// typed text as a case-insensitive prefix. An empty filter matches all.
func filterSlashCommands(filter string) []slashCommand {
	filter = strings.ToLower(filter)
	var out []slashCommand
	for _, c := range slashCommands {
		if strings.HasPrefix(strings.TrimPrefix(c.name, "/"), filter) {
			out = append(out, c)
		}
	}
	return out
}

// syncSlashMenu opens, refilters, or closes the menu from the current input. It
// shows only while the command NAME is being typed: a leading "/", no space yet
// (a space means the user has moved on to arguments), a run not active, and at
// least one matching command. Refiltering resets the highlight to the first row.
func (m Model) syncSlashMenu() Model {
	v := m.input.Value()
	if m.running || !strings.HasPrefix(v, "/") || strings.ContainsAny(v, " \t") {
		m.menu = nil
		return m
	}
	items := filterSlashCommands(v[1:])
	if len(items) == 0 {
		m.menu = nil
		return m
	}
	if m.menu == nil {
		m.menu = &slashMenu{}
	}
	m.menu.items = items
	m.menu.index = 0
	return m
}

// handleSlashMenuKey consumes the navigation/selection keys while the menu is
// open (Up/Down move, Enter/Tab select, Esc close). handled=false lets a key fall
// through to normal input handling (typing refilters via syncSlashMenu).
func (m Model) handleSlashMenuKey(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	switch msg.Type {
	case tea.KeyUp:
		m.menu.index--
		if m.menu.index < 0 {
			m.menu.index = len(m.menu.items) - 1
		}
		return m, nil, true
	case tea.KeyDown:
		m.menu.index++
		if m.menu.index >= len(m.menu.items) {
			m.menu.index = 0
		}
		return m, nil, true
	case tea.KeyEsc:
		m.menu = nil // close, but keep the typed text
		return m, nil, true
	case tea.KeyTab:
		nm, cmd := m.selectSlashCommand(m.menu.items[m.menu.index], false)
		return nm, cmd, true
	case tea.KeyEnter:
		c := m.menu.items[m.menu.index]
		// When the typed text is an exact match for ANY menu item (not just the
		// highlighted one), run it directly — so "/mode" submits as /mode even
		// when /model is highlighted at index 0. Without this, "/mode" would
		// incorrectly select /model because both start with "mode" and /model
		// sorts first in the filtered list.
		typed := strings.TrimSpace(m.input.Value())
		for _, item := range m.menu.items {
			if typed == item.name {
				m.menu = nil
				nm, cmd := m.submit()
				return nm, cmd, true
			}
		}
		nm, cmd := m.selectSlashCommand(c, true)
		return nm, cmd, true
	}
	return m, nil, false
}

// selectSlashCommand applies a menu choice. A no-arg command picked with Enter
// executes immediately; everything else inserts "/<command> " into the input so
// the user can type arguments. The menu closes either way.
func (m Model) selectSlashCommand(c slashCommand, viaEnter bool) (tea.Model, tea.Cmd) {
	m.menu = nil
	if viaEnter && c.noArg {
		m.input.Reset()
		return m.runSlash(c.name)
	}
	m.input.SetValue(c.name + " ")
	m.input.CursorEnd()
	return m, nil
}

// renderSlashMenu draws the inline command menu as a bordered list above the
// input, mirroring the model picker's lipgloss conventions (accent border,
// highlighted row in accent, dim descriptions + footer).
func (m Model) renderSlashMenu() string {
	if m.menu == nil {
		return ""
	}
	lines := make([]string, 0, len(m.menu.items)+1)
	for i, c := range m.menu.items {
		row := c.name + "  " + c.desc
		if i == m.menu.index {
			lines = append(lines, accent.Render("> "+row))
		} else {
			lines = append(lines, "  "+c.name+"  "+muted.Render(c.desc))
		}
	}
	lines = append(lines, muted.Render("↑/↓ move  Enter select  Tab insert  Esc close"))
	return lipgloss.NewStyle().
		Border(lipgloss.NormalBorder()).
		BorderForeground(accentColor).
		Width(max(m.width-2, 20)).
		Render(strings.Join(lines, "\n"))
}
