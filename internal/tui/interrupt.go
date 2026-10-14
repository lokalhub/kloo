package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// handleInterruptKeys implements the idle-vs-running key distinction:
//   - Ctrl-C / Esc while a run is ACTIVE → interrupt the run (cancel ctx); does
//     NOT quit the program.
//   - Esc while the run is ALREADY STOPPING → drop it (back to the prompt now).
//   - Ctrl-C while the run is already stopping → quit the program.
//   - Ctrl-C from IDLE → quit the program.
//   - `q` from IDLE with an empty input → quit (so typing a task containing 'q'
//     still works); Esc from idle is a no-op.
//
// The second-press rules are the point of the key, not a nicety. Cancelling the
// run's context is all the FIRST press can do, and how long the loop then takes to
// unwind is not up to kloo: an endpoint that keeps the socket open, a tool wedged
// in a syscall, an MCP server ignoring its context. Before this, that wait had no
// exit — the screen kept animating the same verb, a second Esc did nothing at all
// (interrupt() had already dropped the cancel func), and Ctrl-C could not quit
// either, because Ctrl-C only quits when `running` is false. Pressing the
// interrupt key and having nothing happen is the whole complaint.
//
// Returns handled=false to let normal key handling proceed.
func (m Model) handleInterruptKeys(msg tea.KeyMsg) (handled bool, nm tea.Model, cmd tea.Cmd) {
	switch msg.Type {
	case tea.KeyCtrlC:
		// A run that was asked to stop and hasn't: Ctrl-C now means quit kloo, which
		// is the one escape that cannot be blocked by the run.
		if m.interrupting {
			return true, m, tea.Quit
		}
		if m.running {
			nm, cmd = m.interrupt()
			return true, nm, cmd
		}
		return true, m, tea.Quit
	case tea.KeyEsc:
		if m.interrupting {
			return true, m.dropRun(), nil
		}
		if m.running {
			nm, cmd = m.interrupt()
			return true, nm, cmd
		}
		return true, m, nil // idle Esc: no-op
	case tea.KeyRunes:
		if string(msg.Runes) == "q" && !m.running && m.input.Value() == "" {
			return true, m, tea.Quit
		}
	}
	return false, m, nil
}

// interrupt cancels the active run's context. The run then returns a
// reportMsg with reason "interrupted", which handleReport renders and returns
// the UI to idle.
//
// If an edit is currently HELD for an approve-each confirm, the pending
// responder is resolved (rejected) and the confirm cleared — otherwise the loop
// goroutine, blocked waiting for y/n, would never unblock and the run would
// deadlock (loop.Run never returns, the program becomes unquittable). This
// complements the run ctx-cancel, which the OnBeforeEdit gate also selects on.
func (m Model) interrupt() (tea.Model, tea.Cmd) {
	if m.confirm != nil {
		if m.confirm.respond != nil {
			go m.confirm.respond(false) // unblock the held edit (rejected)
		}
		m.confirm = nil
	}
	if m.cancel != nil {
		m.cancel() // idempotent: the func is KEPT so a repeat press still reaches it
	}
	// Say so on screen, immediately. The cancel is instant; the loop unwinding from
	// it is not, and until the report lands the only thing the user had to go on was
	// a thinking line still animating the same verb with the clock still climbing —
	// indistinguishable from the key having done nothing.
	m.interrupting = true
	m.interruptAt = time.Now()
	m.activity = "" // the in-flight phrase is no longer what kloo is doing
	return m, nil
}

// dropRun is the second Esc: stop WAITING for the run and hand the prompt back.
//
// The run goroutine is still out there — kloo cannot reach into a blocked syscall —
// so the run is ABANDONED rather than stopped: its context stays cancelled, its
// late output is ignored (see Update), and the report it eventually sends is
// discarded instead of printing a stop banner over a conversation the user has
// moved on from. A new task is refused until it reports, because one Loop is shared
// across submissions and a second run would race the first on its hooks.
func (m Model) dropRun() Model {
	if m.cancel != nil {
		m.cancel()
	}
	m.running, m.interrupting = false, false
	m.abandoned = true
	m.streamIdx = -1
	m.activity = ""
	return m.appendItem(interruptItem{dropped: true})
}

// dropLateRunMsg reports whether msg is a signal from an abandoned run, and
// returns the model with any obligation that message carried discharged.
func (m Model) dropLateRunMsg(msg tea.Msg) (tea.Model, bool) {
	switch msg := msg.(type) {
	case streamDeltaMsg, streamDoneMsg, toolEventMsg, noticeMsg, progressMsg, memoryMsg, contextMsg:
		return m, true
	case confirmRequestMsg:
		if msg.Respond != nil {
			go msg.Respond(false) // never leave the orphaned loop blocked on a y/n
		}
		return m, true
	}
	return m, false
}

// reportMsg is the terminal outcome of a run, translated from the Phase-04
// agent.Report by the runner (keeping the TUI decoupled from internal/agent).
type reportMsg struct {
	Reason     string // success | budget-exceeded | churn | error | interrupted
	Steps      int
	Tokens     int
	MaxTokens  int
	Elapsed    string
	Detail     string // e.g. "max-steps (40/40)" or "same failure ×3"
	VerifyCmd  string
	VerifyExit int
	RolledBack bool
	// Summary is the text the model passed to the finish tool. On a question or
	// review task it IS the answer, and it used to be dropped here: the model
	// composed a reply, kloo captured it on the Report, and the terminal showed a
	// receipt (steps/tokens/elapsed) with the answer nowhere on screen. Reported as
	// kloo "not feeling like" grok or Claude on the same model and question.
	Summary string
	// Rails is the run's soft-rail tally, rendered as "self-corrections: name×n".
	// These were headless-JSON-only, so an interactive user had no way to see that a
	// rail had fired — including chat-gate-action, which means the conversational
	// gate tried to DO the work in a turn that has no tools.
	Rails string
	// SummarySalvaged: the Summary is the last thing the model happened to say, not
	// a reply it was asked for. It may be a fragment mid-investigation, so it is
	// labelled rather than presented as a conclusion — passing one off as a final
	// answer would be worse than showing nothing.
	SummarySalvaged bool
}

// handleReport renders the terminal state and returns the UI to idle. A
// user-initiated interrupt renders a simple line; an autonomous stop (budget /
// churn / error / success) renders a distinct terminal banner.
func (m Model) handleReport(msg reportMsg) (tea.Model, tea.Cmd) {
	m.running = false
	m.interrupting = false
	m.cancel = nil
	m.streamIdx = -1
	m.activity = "" // an in-flight phrase must not outlive the run (task 06)
	// A run the user already dropped (second Esc) has had its outcome rendered;
	// printing its stop banner now would land under whatever they did next.
	if m.abandoned {
		m.abandoned = false
		return m, nil
	}
	if msg.Reason == "interrupted" {
		return m.appendItem(interruptItem{}), nil
	}
	return m.appendItem(reportItem{r: msg, verbose: m.verbose}), nil
}

// reportItem renders a run's outcome. Two forms: the default one, which is the
// answer and — only when there is something to say — one plain sentence about why
// kloo stopped; and the /verbose one, the original counter banner.
//
// The banner used to be unconditional, so EVERY run ended in a bordered receipt:
//
//	■ run stopped — BUDGET EXCEEDED
//	reason:  steps (40/40)
//	steps:   40 · tokens: 1.2k/200k · elapsed: 3m04s
//	self-corrections: explore-nudge×2, repeat-corrective×1
//
// Four lines of kloo's internals under every answer, including the ones that went
// perfectly. None of it is what the user asked for, the ratios advertise rails they
// will almost never hit, and "BUDGET EXCEEDED" over a finished task reads as a
// failure. Claude and grok end a turn with the reply and nothing else; so does kloo
// now. The numbers are unchanged in the headless JSON (which is what kloo-bench
// reads) and one `/verbose` away on screen.
type reportItem struct {
	r       reportMsg
	verbose bool
}

func (ri reportItem) render(width int) string {
	if !ri.verbose {
		return ri.renderQuiet(width)
	}
	r := ri.r
	title := bannerTitle(r.Reason)

	var b strings.Builder
	// The answer comes FIRST. A summary is what the model wanted to tell the user;
	// the counters below are bookkeeping, and burying prose under them (or dropping
	// it, as this did) is what made a question to kloo feel unanswered.
	if sum := strings.TrimSpace(r.Summary); sum != "" {
		if r.SummarySalvaged {
			b.WriteString("last thing kloo said (not a final answer):\n")
		}
		b.WriteString(sum)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "■ run stopped — %s\n", title)
	if r.Detail != "" {
		fmt.Fprintf(&b, "reason:  %s\n", r.Detail)
	}
	tokens := human(r.Tokens) // maxTokens 0 ⇒ unbounded: plain counter, not "N/0"
	if r.MaxTokens > 0 {
		tokens = human(r.Tokens) + "/" + human(r.MaxTokens)
	}
	fmt.Fprintf(&b, "steps:   %d · tokens: %s · elapsed: %s\n", r.Steps, tokens, r.Elapsed)
	if r.VerifyCmd != "" {
		// Colour the verify outcome green pass / red fail (task 05), consistent
		// with the run-command exit result.
		result := verifyPass.Render(fmt.Sprintf("exit %d %s", r.VerifyExit, glyphPass))
		if r.VerifyExit != 0 {
			result = verifyFail.Render(fmt.Sprintf("exit %d %s", r.VerifyExit, glyphFail))
		}
		fmt.Fprintf(&b, "last verify: %s → %s\n", r.VerifyCmd, result)
	}
	if r.Rails != "" {
		fmt.Fprintf(&b, "self-corrections: %s\n", r.Rails)
	}
	if r.RolledBack {
		b.WriteString("workspace rolled back to checkpoint. Refine the task and retry.")
	}

	border := lipgloss.NormalBorder()
	style := cardStyle(width, border)
	// success and answered are calm outcomes (no attention tint); the rest get the
	// warning border. "answered" = the model replied conversationally, not a failure.
	if r.Reason != "success" && r.Reason != "answered" {
		style = style.BorderForeground(warningColor) // attention tint (theme.go)
	}
	return style.Render(strings.TrimRight(b.String(), "\n"))
}

// renderQuiet is the default outcome: the answer, then at most one sentence.
//
// No border and no card. A card draws the eye to the frame, and on a finished task
// the thing worth reading is the prose inside it.
func (ri reportItem) renderQuiet(width int) string {
	r := ri.r
	var b strings.Builder
	if sum := strings.TrimSpace(r.Summary); sum != "" {
		if r.SummarySalvaged {
			b.WriteString(muted.Render("kloo stopped before it answered — the last thing it said:") + "\n")
		}
		b.WriteString(wrapLines(sum, width-2))
	}
	if note := quietStopNote(r); note != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(hangingNote(warning.Render("■ "), note, width-2))
	}
	if r.RolledBack {
		b.WriteString("\n" + hangingNote(warning.Render("■ "), "the workspace was put back the way it was before this run.", width-2))
	}
	// The dim footer carries the two facts that are still worth a glance: how long it
	// took, and a verify that is RED (actionable — it names the command to re-run). A
	// green verify says nothing the answer above does not.
	foot := make([]string, 0, 2)
	if e := strings.TrimSpace(r.Elapsed); e != "" && e != "0s" {
		foot = append(foot, e)
	}
	if r.VerifyCmd != "" && r.VerifyExit != 0 {
		foot = append(foot, fmt.Sprintf("%s → exit %d %s", r.VerifyCmd, r.VerifyExit, glyphFail))
	}
	if len(foot) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(muted.Render(strings.Join(foot, " · ")))
	}
	return strings.TrimRight(b.String(), "\n")
}

// quietStopNote is the one sentence a non-success outcome earns, in words that
// describe what happened to the USER's task and what to do about it — not which
// internal counter tripped. "" when the run simply finished: a completed task needs
// no commentary, and the loudest thing kloo did was comment on its own successes.
func quietStopNote(r reportMsg) string {
	switch r.Reason {
	case "success", "answered", "interrupted":
		// interrupted is rendered by interruptItem; the others have nothing to add.
		return ""
	case "budget-exceeded":
		// The memory ceiling is a different event from running long, and it is the one
		// case where kloo stopping is protecting the machine rather than the task.
		if strings.HasPrefix(r.Detail, "memory") {
			return "kloo stopped itself to keep from running this machine out of memory. The work so far is kept."
		}
		return `kloo stopped itself before finishing this task. Type "continue" to pick up where it left off.`
	case "churn":
		return "kloo was repeating itself without making progress, so it stopped. A more specific instruction — which file, which test — usually unsticks it."
	case "explore-stop":
		return `kloo kept reading without changing anything, so it stopped. Naming the file to edit usually gets it moving.`
	case "unverified":
		// Not a failure: there was no build/test command to gate on, so the work is
		// done but unproven. Saying nothing would imply it was checked.
		return "done, but there was no build or test command to check it against — nothing verified this."
	default:
		// error / malformed-tool-call / anything new: Detail is already a sentence with
		// a hint (humanizeError), which is exactly what belongs here.
		if d := strings.TrimSpace(r.Detail); d != "" {
			return d
		}
		return "kloo stopped before finishing this task."
	}
}

func bannerTitle(reason string) string {
	switch reason {
	case "budget-exceeded":
		return "BUDGET EXCEEDED"
	case "churn":
		return "NO PROGRESS (churn)"
	case "error":
		return "ERROR"
	case "success":
		return "COMPLETE"
	case "answered":
		return "ANSWERED"
	default:
		return strings.ToUpper(reason)
	}
}
