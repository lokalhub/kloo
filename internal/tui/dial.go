package tui

import (
	tea "github.com/charmbracelet/bubbletea"
)

// The permission dial has three settings (default auto, design doc §2):
//
//   - auto         — edits apply without a prompt (the loop runs unattended).
//   - accept-edits — v1 semantics: identical to auto for edits (edits apply
//     automatically). It is a distinct, named setting reserved for future
//     divergence (e.g. auto-applying edits but prompting before run_command);
//     in v1 it does NOT prompt. Documented in decisions.md.
//   - approve-each — debug mode: before each edit the loop pauses and the TUI
//     renders the diff card with a confirm prompt; the run blocks on that edit
//     until the user answers y/n.
//
// The dial value lives on the model (m.mode), is set by /mode (commands.go), and
// is shown by the status line (status.go). The runner (task 09) reads the mode
// to decide whether to send a confirmRequestMsg (approve-each) or apply directly.

// confirmState holds a pending approve-each confirmation.
type confirmState struct {
	respond func(bool) // called with the user's y/n decision
}

// confirmRequestMsg is sent by the runner in approve-each mode before applying
// an edit: it renders the diff card + a confirm prompt and blocks the edit (the
// runner waits on respond) until the user answers. The bridge fills Edits with
// the parsed SEARCH/REPLACE blocks; tests may instead set a single Search/Replace.
type confirmRequestMsg struct {
	Path    string
	Edits   []editPair
	Search  string
	Replace string
	Respond func(bool)
}

// handleConfirmRequest renders the held edit (as a proper -/+ diff card) + a
// confirm prompt and records the responder so the next y/n answers it.
func (m Model) handleConfirmRequest(msg confirmRequestMsg) (tea.Model, tea.Cmd) {
	edits := msg.Edits
	if len(edits) == 0 && (msg.Search != "" || msg.Replace != "") {
		edits = []editPair{{search: msg.Search, replace: msg.Replace}}
	}
	m = m.appendItem(editCardItem{path: msg.Path, edits: edits})
	m = m.appendItem(infoItem{text: "apply this edit? [y/n]"})
	m.confirm = &confirmState{respond: msg.Respond}
	return m, nil
}

// handleConfirmKey consumes y/n while a confirm is pending.
func (m Model) handleConfirmKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "y", "Y":
		respond := m.confirm.respond
		m.confirm = nil
		m = m.appendItem(infoItem{text: "edit applied"})
		if respond != nil {
			go respond(true)
		}
		return m, nil
	case "n", "N":
		respond := m.confirm.respond
		m.confirm = nil
		m = m.appendItem(infoItem{text: "edit rejected"})
		if respond != nil {
			go respond(false)
		}
		return m, nil
	}
	// Ignore other keys while a confirm is pending.
	return m, nil
}
