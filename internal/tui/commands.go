package tui

import (
	"context"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// isSlash reports whether a submitted line is a slash command.
func isSlash(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), "/")
}

// runSlash parses and dispatches a slash command, taking effect on state. An
// unknown slash produces a clear visible message and submits nothing.
func (m Model) runSlash(line string) (tea.Model, tea.Cmd) {
	fields := strings.Fields(strings.TrimSpace(line))
	cmd := fields[0]
	arg := ""
	if len(fields) > 1 {
		arg = strings.Join(fields[1:], " ")
	}

	switch cmd {
	case "/add":
		return m.slashAdd(arg), nil
	case "/model":
		return m.slashModel(arg), nil
	case "/mode":
		return m.slashMode(arg), nil
	case "/stop":
		return m.slashStop()
	case "/diff":
		return m.slashDiff(), nil
	default:
		return m.appendItem(infoItem{text: "unknown command: " + cmd}), nil
	}
}

func (m Model) slashAdd(path string) Model {
	if strings.TrimSpace(path) == "" {
		return m.appendItem(infoItem{text: "/add needs a path"})
	}
	m.contextFiles = append(append([]string{}, m.contextFiles...), path)
	return m.appendItem(infoItem{text: "added " + path + " to context"})
}

func (m Model) slashModel(name string) Model {
	name = strings.TrimSpace(name)
	if name == "" {
		return m.appendItem(infoItem{text: "/model needs a model name"})
	}
	// kloo is BYO-endpoint: accept any model name the configured endpoint serves.
	m.modelName = name
	m.status.model = name
	return m.appendItem(infoItem{text: "model: " + name})
}

func (m Model) slashMode(value string) Model {
	value = strings.TrimSpace(value)
	switch Mode(value) {
	case ModeAuto, ModeAcceptEdits, ModeApproveEach:
		m.mode = Mode(value)
		m.status.mode = m.mode
		return m.appendItem(infoItem{text: "mode: " + value})
	default:
		return m.appendItem(infoItem{text: "invalid mode: " + value + " (valid: auto, accept-edits, approve-each)"})
	}
}

func (m Model) slashStop() (tea.Model, tea.Cmd) {
	if !m.running {
		return m.appendItem(infoItem{text: "nothing to stop"}), nil
	}
	// Delegate to the interrupt path (task 07).
	return m.interrupt()
}

func (m Model) slashDiff() Model {
	if len(m.pendingDiffs) == 0 {
		return m.appendItem(infoItem{text: "no pending diff"})
	}
	m = m.appendItem(infoItem{text: "pending diff:"})
	for _, d := range m.pendingDiffs {
		m = m.appendItem(d)
	}
	return m
}

// submitTask appends the user line and, if a Runner is wired, launches an
// autonomous run under a cancelable context. Without a Runner (unit tests) it
// just records the submission.
//
// One run at a time: a non-slash submission is ignored (with a message) while a
// run is active — otherwise a second goroutine would race the first on the
// shared loop's hooks and overwrite the cancel func, leaving the first run
// uninterruptible (mirrors the running-check /stop has).
func (m Model) submitTask(line string) (tea.Model, tea.Cmd) {
	if m.running {
		return m.appendItem(infoItem{text: "a run is already active — press Esc to interrupt it first"}), nil
	}
	m = m.appendItem(userItem{text: line})
	if m.runner == nil {
		return m, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.running = true
	m.runStart = time.Now()
	m.verb = randomVerb()
	m.tickCount = 0
	files := append([]string{}, m.contextFiles...)
	mode := m.mode
	runner := m.runner
	return m, tea.Batch(
		func() tea.Msg {
			go runner.Start(ctx, line, mode, files)
			return nil
		},
		tickCmd(),
	)
}
