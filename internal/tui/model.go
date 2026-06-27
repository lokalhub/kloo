// Package tui is kloo's Claude-Code-style terminal UI (Bubble Tea). It drives the
// Phase-04 autonomous loop: a header/status line, a scrolling transcript with
// streaming assistant text and tool/diff cards, and an input region with slash
// commands, a permission dial (default auto), and Esc/Ctrl-C interrupt.
//
// The input is abstracted behind a TaskSource seam (source.go) so a future
// stdin/J1-provider source can be added without changing the model; that source
// is designed, not wired, in v1.
package tui

import (
	"context"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/session"
)

// Mode is the permission dial setting (dial.go owns its behaviour). The default
// is auto (design doc §2 / master plan §5).
type Mode string

const (
	ModeAuto        Mode = "auto"
	ModeAcceptEdits Mode = "accept-edits"
	ModeApproveEach Mode = "approve-each"
)

// Model is the root Bubble Tea model. It is a value type (Bubble Tea reassigns
// it each Update); slice fields are append-and-reassign so copies don't alias.
type Model struct {
	width, height int

	input   textinput.Model
	vp      viewport.Model
	vpReady bool

	// transcript is the ordered list of rendered items (user/assistant/cards/…).
	transcript []item
	// streamIdx is the index of the in-progress streaming assistant item, or -1.
	streamIdx int

	// status-line state (status.go).
	status statusData
	mode   Mode

	// run state (interrupt.go / wiring).
	running bool
	cancel  context.CancelFunc

	// activity is the display-only in-flight phrase ("editing <file>", "running
	// <cmd>") shown in the thinking line, set from each tool event (task 06).
	activity string
	// expanded toggles full vs truncated run-command output (ctrl+o, task 03).
	expanded bool

	// thinking-line animation (verbs.go): a rotating verb + spinner shown while
	// a run is in flight, Claude-Code style.
	verb      string
	spinFrame int
	tickCount int
	runStart  time.Time

	// approve-each confirm state (dial.go).
	confirm *confirmState

	// version is the build version shown in the header lead (set from Config).
	version string

	// model name + context files (slash commands).
	modelName    string
	contextFiles []string
	modelLister  ModelLister
	modelOptions []ModelOption
	picker       *modelPicker
	runtime      RuntimeConfig

	// menu is the inline, filterable slash-command menu shown above the input when
	// the current line starts with "/" and no run is active (slash_menu.go); nil
	// when closed.
	menu *slashMenu

	// pastes holds long/multi-line pastes collapsed to placeholders in the input
	// (paste.go); expanded to full text when the task is submitted.
	pastes []pastedText

	// source is the v1 task source (keyboard); the seam admits a future stdin
	// source (source.go).
	source TaskSource

	// runner launches the autonomous loop for a submitted task (wiring, task 09);
	// nil in unit tests that don't exercise a real run.
	runner Runner
}

// Config seeds the model's initial display state.
type Config struct {
	Version   string     // build version shown in the header ("dev" for a local build)
	Effort    string     // effort tier for the status line (fast|medium|heavy)
	Model     string     // model name for the status line
	MaxSteps  int        // step budget (status line "step N/max")
	MaxTokens int        // token budget (status line "used/budget")
	Runner    Runner     // optional: launches a real run on task submit
	Source    TaskSource // optional: defaults to the keyboard source
	Banner    string     // optional: a startup notice shown in the transcript (e.g. "resumed session …")
	ModelList ModelLister
	Provider  string
	Endpoint  string
	APIKey    string
	// Runtime knobs applied to the next run and updated by /model switches.
	ContextTokens int
	Temperature   float64
	ToolFormat    string
	NoThink       bool
	NoThinkLocked bool
	NewClient     func(endpoint, model, apiKey string) llm.LLMClient
	// History is the prior conversation to replay on resume (compact display items
	// from the saved session). Rendered above the banner so a resumed session shows
	// what happened before, not just a one-line notice. Empty for a fresh session.
	History []session.DisplayItem
}

// New constructs the root model in its idle state.
func New(cfg Config) Model {
	in := textinput.New()
	in.Placeholder = "type a task…"
	in.Prompt = "> "
	in.Focus()

	modelName := cfg.Model
	if modelName == "" {
		modelName = "local"
	}
	useNewClient := cfg.NewClient != nil
	newClient := cfg.NewClient
	if newClient == nil {
		newClient = func(endpoint, model, apiKey string) llm.LLMClient {
			return llm.New(endpoint, model, llm.WithAPIKey(apiKey))
		}
	}
	runtime := RuntimeConfig{
		Provider:      cfg.Provider,
		Endpoint:      cfg.Endpoint,
		APIKey:        cfg.APIKey,
		Model:         modelName,
		ContextTokens: cfg.ContextTokens,
		Temperature:   cfg.Temperature,
		ToolFormat:    cfg.ToolFormat,
		NoThink:       cfg.NoThink,
		NoThinkLocked: cfg.NoThinkLocked,
		NewClient:     newClient,
		UseNewClient:  useNewClient,
	}
	if runtime.ContextTokens == 0 {
		runtime.ContextTokens = config.DefaultMaxContextTokens
	}
	if runtime.ToolFormat == "" {
		runtime.ToolFormat = config.DefaultToolFormat
	}

	m := Model{
		input:     in,
		streamIdx: -1,
		mode:      ModeAuto,
		version:   cfg.Version,
		modelName: modelName,
		status: statusData{
			effort:    cfg.Effort,
			model:     modelName,
			provider:  cfg.Provider,
			maxSteps:  cfg.MaxSteps,
			maxTokens: cfg.MaxTokens,
			mode:      ModeAuto,
		},
		runner:      cfg.Runner,
		modelLister: cfg.ModelList,
		runtime:     runtime,
	}
	m.source = cfg.Source
	if m.source == nil {
		m.source = keyboardSource{} // the one v1 implementation of TaskSource
	}
	// Replay the prior conversation (resume), then the banner as the boundary line
	// between "earlier" and the live prompt.
	for _, d := range cfg.History {
		m = m.appendItem(displayItemToTranscript(d))
	}
	if cfg.Banner != "" {
		m = m.appendItem(infoItem{text: cfg.Banner}) // e.g. "resumed session …"
	}
	return m
}

// displayItemToTranscript maps a saved session display item back to a rendered
// transcript block: prompts and assistant prose render as their live cards; a tool
// summary renders as a dim info line (the one-line action, not the live diff card).
func displayItemToTranscript(d session.DisplayItem) item {
	switch d.Kind {
	case dispUser:
		return userItem{text: d.Text}
	case dispAssistant:
		return assistantItem{content: d.Text}
	default: // dispTool (and any unknown kind) → a dim one-line action
		return infoItem{text: "↳ " + d.Text}
	}
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(textinput.Blink, m.source.Attach())
}

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.resize(msg.Width, msg.Height)
	case tea.MouseMsg:
		// Mouse wheel scrolls the transcript viewport (scrollback).
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case tea.KeyMsg:
		return m.handleKey(msg)
	case streamDeltaMsg:
		return m.handleStreamDelta(msg)
	case streamDoneMsg:
		return m.handleStreamDone(msg)
	case toolEventMsg:
		return m.handleToolEvent(msg)
	case noticeMsg:
		return m.appendItem(infoItem{text: msg.text}), nil
	case progressMsg:
		return m.handleProgress(msg)
	case memoryMsg:
		return m.handleMemory(msg)
	case clipboardMsg:
		return m.appendItem(infoItem{text: fmt.Sprintf("copied %d chars to clipboard", msg.chars)}), nil
	case tickMsg:
		return m.handleTick(msg)
	case reportMsg:
		return m.handleReport(msg)
	case confirmRequestMsg:
		return m.handleConfirmRequest(msg)
	case submitTaskMsg:
		return m.submitTask(msg.task)
	}

	// Default: let the focused input handle the message.
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// resize lays out the regions for a new window size.
func (m Model) resize(w, h int) (tea.Model, tea.Cmd) {
	m.width, m.height = w, h
	// The input box spans the full line (the slash-command menu floats above it).
	m.input.Width = w - 6

	vpHeight := h - headerHeight - activityHeight - inputHeight - hintHeight
	if vpHeight < 1 {
		vpHeight = 1
	}
	if !m.vpReady {
		m.vp = viewport.New(w-2, vpHeight)
		m.vpReady = true
	} else {
		m.vp.Width = w - 2
		m.vp.Height = vpHeight
	}
	atBottom := m.vp.AtBottom()
	m.vp.SetContent(m.transcriptContent())
	if atBottom { // sticky bottom: a resize must not yank a scrolled-up user back down
		m.vp.GotoBottom()
	}
	return m, nil
}

// handleKey handles key input. The idle-vs-running distinction (Esc/Ctrl-C) is
// in interrupt.go; slash submission is in commands.go.
func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.picker != nil {
		return m.handleModelPickerKey(msg)
	}
	// The inline slash-command menu (slash_menu.go) consumes its navigation keys
	// before the interrupt/Esc path so Esc closes the menu (idle Esc is otherwise a
	// no-op). It only opens while !running, so this never shadows run interrupt.
	if m.menu != nil {
		if nm, cmd, handled := m.handleSlashMenuKey(msg); handled {
			return nm, cmd
		}
	}
	// Interrupt / quit keys are mode-sensitive (interrupt.go).
	if handled, nm, cmd := m.handleInterruptKeys(msg); handled {
		return nm, cmd
	}
	// approve-each confirm consumes y/n while a confirm is pending (dial.go).
	if m.confirm != nil {
		return m.handleConfirmKey(msg)
	}

	// A long/multi-line bracketed paste collapses to a placeholder instead of
	// flooding the input (paste.go). Short pastes fall through to the input.
	if msg.Paste {
		if nm, handled := m.handlePaste(string(msg.Runes)); handled {
			return nm, nil
		}
	}

	switch msg.Type {
	case tea.KeyPgUp, tea.KeyPgDown:
		// Page the transcript viewport for scrollback (the input never uses these,
		// so they're safe to hijack even while typing). Mouse wheel also works.
		var cmd tea.Cmd
		m.vp, cmd = m.vp.Update(msg)
		return m, cmd
	case tea.KeyCtrlY:
		// Copy the last assistant reply to the system clipboard (OSC 52). No external
		// tool, works over SSH. Shift+drag native selection is the fallback where the
		// terminal doesn't support OSC 52.
		text := m.lastAssistantText()
		if text == "" {
			return m.appendItem(infoItem{text: "nothing to copy yet"}), nil
		}
		return m, copyToClipboard(text)
	case tea.KeyEnter:
		return m.submit()
	case tea.KeyCtrlO:
		// Toggle truncated ↔ full run-command output (task 03) and refresh the
		// cached viewport content immediately, mirroring resize.
		m.expanded = !m.expanded
		if m.vpReady {
			m.vp.SetContent(m.transcriptContent())
		}
		return m, nil
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	m = m.syncSlashMenu() // open/refilter/close the slash-command menu on input change
	return m, cmd
}

// submit routes the current input line: a slash command (commands.go) or a task
// submission via the active TaskSource.
func (m Model) submit() (tea.Model, tea.Cmd) {
	line := m.input.Value()
	m.input.Reset()
	if line == "" {
		return m, nil
	}
	if isSlash(line) {
		return m.runSlash(line)
	}
	// Route non-slash input through the task-submission seam (submitTaskMsg) —
	// the single channel every TaskSource feeds.
	return m, func() tea.Msg { return submitTaskMsg{task: line} }
}
