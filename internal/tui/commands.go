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

// ackReplies maps a bare acknowledgment / closing to a short conversational reply.
// A weak model, handed a no-op input like "thanks" on a RESUMED session (where the
// finished task is still in context), tends to re-interpret it as "do the task
// again" and launches a fresh tool-driven run — even though the system prompt tells
// it to just finish. So kloo short-circuits the obvious cases deterministically:
// reply in one line, launch NO run. Kept to UNMISTAKABLE whole-message acks (a
// real request like "thanks, now add a tab" is longer and won't match), so we never
// swallow actual work.
var ackReplies = map[string]string{
	"thanks":           "You're welcome! Let me know if you'd like any changes or another task.",
	"thank you":        "You're welcome! Let me know if you'd like any changes or another task.",
	"thank u":          "You're welcome! Let me know if you'd like any changes or another task.",
	"thanks so much":   "You're welcome! Let me know if you'd like any changes or another task.",
	"thanks a lot":     "You're welcome! Let me know if you'd like any changes or another task.",
	"ty":               "You're welcome! Let me know if you'd like any changes or another task.",
	"thx":              "You're welcome! Let me know if you'd like any changes or another task.",
	"tysm":             "You're welcome! Let me know if you'd like any changes or another task.",
	"much appreciated": "Anytime! Happy to help with the next thing.",
	"appreciate it":    "Anytime! Happy to help with the next thing.",
	"appreciated":      "Anytime! Happy to help with the next thing.",
	"cheers":           "Anytime! Happy to help with the next thing.",
	"great":            "Glad it helped! Happy to make tweaks or take on the next thing.",
	"great work":       "Glad it helped! Happy to make tweaks or take on the next thing.",
	"nice":             "Glad it helped! Happy to make tweaks or take on the next thing.",
	"nice work":        "Glad it helped! Happy to make tweaks or take on the next thing.",
	"perfect":          "Glad it helped! Happy to make tweaks or take on the next thing.",
	"awesome":          "Glad it helped! Happy to make tweaks or take on the next thing.",
	"excellent":        "Glad it helped! Happy to make tweaks or take on the next thing.",
	"good job":         "Glad it helped! Happy to make tweaks or take on the next thing.",
	"well done":        "Glad it helped! Happy to make tweaks or take on the next thing.",
	"looks good":       "Glad it helped! Happy to make tweaks or take on the next thing.",
	"lgtm":             "Glad it helped! Happy to make tweaks or take on the next thing.",
	"ok":               "👍 I'm here whenever you want to continue.",
	"okay":             "👍 I'm here whenever you want to continue.",
	"k":                "👍 I'm here whenever you want to continue.",
	"kk":               "👍 I'm here whenever you want to continue.",
	"alright":          "👍 I'm here whenever you want to continue.",
	"got it":           "👍 I'm here whenever you want to continue.",
	"sounds good":      "👍 I'm here whenever you want to continue.",
	"all good":         "👍 I'm here whenever you want to continue.",
	"no thanks":        "👍 I'm here whenever you want to continue.",
	"that's all":       "👍 I'm here whenever you want to continue.",
	"bye":              "👋 Bye for now — reopen the session anytime to continue.",
	"goodbye":          "👋 Bye for now — reopen the session anytime to continue.",
}

// ackReply returns a canned reply if the line is a bare acknowledgment / closing,
// else ("", false). Matching is on the message reduced to lowercase letters +
// single spaces (so "Thanks!", "thank you :)", "thanks 🙏" all match), guarding
// against catching a real request by only honouring the exact whole-message acks.
func ackReply(line string) (string, bool) {
	var b strings.Builder
	prevSpace := false
	for _, r := range strings.ToLower(strings.TrimSpace(line)) {
		switch {
		case r >= 'a' && r <= 'z', r == '\'':
			b.WriteRune(r)
			prevSpace = false
		case r == ' ' || r == '\t':
			if !prevSpace && b.Len() > 0 {
				b.WriteRune(' ')
			}
			prevSpace = true
		}
		// everything else (punctuation, emoji, digits) is dropped
	}
	reply, ok := ackReplies[strings.TrimSpace(b.String())]
	return reply, ok
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
	case "/models":
		return m.slashModels(), nil
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
		return m.openModelPicker()
	}
	return m.selectModelName(name)
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
	m = m.appendItem(userItem{text: line}) // transcript shows the line as typed (paste placeholders kept short)
	// A bare "thanks"/"ok"/"nice" is not a task — reply in one line and launch NO
	// run, so a weak model can't mistake it for "redo the work" (which it did even
	// with the system prompt telling it to just finish).
	if reply, ok := ackReply(line); ok {
		return m.appendItem(assistantItem{content: reply}), nil
	}
	if m.runner == nil {
		return m, nil
	}
	task := m.expandPastes(line) // the model receives the full pasted text
	m.pastes = nil               // consumed by this submission
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.running = true
	m.runStart = time.Now()
	m.verb = randomVerb()
	m.tickCount = 0
	files := append([]string{}, m.contextFiles...)
	mode := m.mode
	runtime := m.runtime
	runner := m.runner
	return m, tea.Batch(
		func() tea.Msg {
			go runner.Start(ctx, task, runtime, mode, files)
			return nil
		},
		tickCmd(),
	)
}
