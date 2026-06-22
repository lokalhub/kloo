package tui

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/lokal/kloo/internal/tools"
)

// blockingRunner records that Start was called and how many times; it blocks
// until released so a second submission can be attempted while "running".
type blockingRunner struct {
	starts  int
	release chan struct{}
}

func (r *blockingRunner) Start(ctx context.Context, task string, mode Mode, files []string) {
	r.starts++
	<-r.release
}

// TestSubmitTaskRejectedWhileRunning: a non-slash submission while a run is
// active is ignored (with a message) — it must NOT launch a second run on the
// shared loop (the data-race / uncancelable-run bug).
func TestSubmitTaskRejectedWhileRunning(t *testing.T) {
	runner := &blockingRunner{release: make(chan struct{})}
	defer close(runner.release)

	m := sized(New(Config{Model: "snappy", MaxSteps: 40, MaxTokens: 8000, Runner: runner}), tw, th)

	// First submission starts a run (m.running becomes true).
	tm, cmd := m.Update(submitTaskMsg{task: "first task"})
	m = tm.(Model)
	if cmd != nil {
		_ = cmd() // executes the goroutine launch (runner.Start in a goroutine)
	}
	if !m.running {
		t.Fatalf("first submission should mark the loop running")
	}

	// Second submission while running must be rejected, not launch another run.
	tm2, _ := m.Update(submitTaskMsg{task: "second task"})
	m2 := tm2.(Model)
	if !contains(m2.View(), "a run is already active") {
		t.Errorf("second submission while running should show the busy message:\n%s", m2.View())
	}
	if contains(m2.View(), "▸ you: second task") {
		t.Errorf("the rejected task must not be echoed as accepted")
	}
}

// TestPinnedSectionInjectsAddedFiles: /add-pinned files are read (jailed) and
// injected into the system prompt — so /add actually affects the loop's context.
func TestPinnedSectionInjectsAddedFiles(t *testing.T) {
	root := t.TempDir()
	canon, _ := filepath.EvalSymlinks(root)
	if err := os.WriteFile(filepath.Join(canon, "notes.txt"), []byte("IMPORTANT-PINNED-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}
	ws, err := tools.NewWorkspace(canon)
	if err != nil {
		t.Fatal(err)
	}

	sec := pinnedSection(ws, []string{"notes.txt"})
	if !contains(sec, "notes.txt") || !contains(sec, "IMPORTANT-PINNED-CONTENT") {
		t.Errorf("pinned section should include the file name + content:\n%s", sec)
	}
	// No pins → empty.
	if pinnedSection(ws, nil) != "" {
		t.Errorf("no pinned files should yield an empty section")
	}
	// A path escaping the jail is reported, not read.
	esc := pinnedSection(ws, []string{"../escape.txt"})
	if !contains(esc, "unreadable") {
		t.Errorf("an escaping path should be noted as unreadable, got:\n%s", esc)
	}
}

// TestConfirmGateUnblocksOnCtxCancel: an approve-each gate returns false
// (rejected) when the run ctx is cancelled, so a held edit can never deadlock
// the loop goroutine even if the user never answers y/n.
func TestConfirmGateUnblocksOnCtxCancel(t *testing.T) {
	var got tea.Msg
	r := &LoopRunner{send: func(m tea.Msg) { got = m }}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled → ctx.Done() is ready

	done := make(chan bool, 1)
	go func() {
		done <- r.confirmGate(ctx, tools.Call{Name: "edit_file", Args: map[string]any{"path": "a.ts", "diff": "x"}})
	}()
	select {
	case d := <-done:
		if d != false {
			t.Errorf("cancelled ctx should make the gate return false, got %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("confirmGate did not unblock on ctx cancel → deadlock")
	}
	if _, ok := got.(confirmRequestMsg); !ok {
		t.Errorf("the gate should still send a confirmRequestMsg, got %T", got)
	}
}

// TestConfirmGateReturnsUserDecision: when the user answers, the gate returns it.
func TestConfirmGateReturnsUserDecision(t *testing.T) {
	respCh := make(chan func(bool), 1) // channel handoff avoids a shared-var race
	r := &LoopRunner{send: func(m tea.Msg) {
		if cr, ok := m.(confirmRequestMsg); ok {
			respCh <- cr.Respond
		}
	}}
	done := make(chan bool, 1)
	go func() {
		done <- r.confirmGate(context.Background(), tools.Call{Name: "edit_file", Args: map[string]any{"path": "a"}})
	}()

	var respond func(bool)
	select {
	case respond = <-respCh:
	case <-time.After(2 * time.Second):
		t.Fatal("gate never sent the confirm request")
	}
	respond(true)

	select {
	case d := <-done:
		if d != true {
			t.Errorf("gate should return the user's decision (true), got %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gate did not return after the user answered")
	}
}

// ensure the Runner interface stays satisfied by *LoopRunner (compile guard).
var _ Runner = (*LoopRunner)(nil)
var _ sendSetter = (*LoopRunner)(nil)
var _ tea.Msg = submitTaskMsg{}
