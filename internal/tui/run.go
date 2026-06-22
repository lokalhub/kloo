package tui

import (
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Run constructs and runs the TUI program with the given config. It is the
// production entrypoint (internal/cli invokes it when kloo launches without a
// one-shot task); tests drive the Model directly via teatest instead.
//
// A Runner that needs to push messages into the program implements sendSetter so
// Run can connect the program's Send after the program is built (task 09's
// LoopRunner does this).
type sendSetter interface{ setSend(func(tea.Msg)) }

// When the configured Runner needs a message sink, it is connected to the
// program here so the loop's streaming/tool/progress/report signals reach the UI.
func Run(cfg Config) error {
	// Decide the colour profile once, explicitly, before the program builds: with
	// NO_COLOR set or a non-TTY stdout, force the ascii profile so every theme
	// style resolves to plain text (no ANSI escapes) — the design-doc §D degrade
	// contract. lipgloss/termenv would usually auto-detect this, but an explicit,
	// tested decision makes the contract a guarantee rather than an incidental.
	if wantsNoColor(os.LookupEnv, stdoutIsTTY()) {
		lipgloss.SetColorProfile(termenv.Ascii)
	}

	m := New(cfg)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if ss, ok := cfg.Runner.(sendSetter); ok {
		ss.setSend(p.Send)
	}
	_, err := p.Run()
	return err
}

// wantsNoColor decides whether colour must be disabled. It is pure (no globals)
// so the NO_COLOR × TTY matrix is unit-testable without a real terminal. Colour
// is disabled when NO_COLOR is present in the environment (presence, any value —
// the no-color.org convention, including the empty string) OR stdout is not a
// TTY (piped/redirected output).
func wantsNoColor(lookupEnv func(string) (string, bool), isTTY bool) bool {
	if _, ok := lookupEnv("NO_COLOR"); ok {
		return true
	}
	return !isTTY
}

// stdoutIsTTY reports whether os.Stdout is a character device (a real terminal).
// Pure stdlib (no new dependency): a non-character-device stdout is a pipe/file.
func stdoutIsTTY() bool {
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}
