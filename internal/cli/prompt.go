package cli

import (
	"os"
	"strconv"
	"strings"
)

// defaultSystemPrompt is the system prompt shared by the interactive (tui.go) and
// headless (headless.go) entry points, kept in one place so the two can't drift.
//
// Alignment note: the verify command is kloo's success SIGNAL, not a goal in
// itself. A small model, told to "work until verify passes", will fabricate
// unrequested changes to turn a red verify green — even undoing a destructive
// request (e.g. recreating files the user just asked to delete), because the
// deletion is what made verify fail. The wording below puts the user's intent
// above the verify result and routes conversational turns straight to finish. The
// stronger guarantee is a per-task verify; this prompt is the cheap lever.
const defaultSystemPrompt = "You are kloo, an autonomous coding assistant. Each turn, make exactly one " +
	"tool call to read, edit, or run a command, working toward the user's request. " +
	"Use SEARCH/REPLACE edits; never rewrite whole files. The verify command checks " +
	"your work, but it is NOT a goal in itself: do NOT invent unrequested changes just " +
	"to make it pass, and NEVER undo or redo something the user explicitly asked for " +
	"(e.g. recreating files they told you to delete). If the user's request legitimately " +
	"makes verify fail, say so and stop — do not 'fix' it. When the task is complete, or " +
	"the message is a question, a thanks, or other conversational reply, call the finish " +
	"tool with a short summary instead of running more commands. " +
	"A fast lint may report style/syntax issues on the file you just edited — use it to " +
	"fix obvious mistakes, but it does NOT decide success; only the verify command does."

// grokAlignedSystemPrompt is the action-first prompt (KLOO_GROK_PROMPT=1).
//
// Captured from grok through a logging proxy (docs/apps/kloo/plans/beat-grok/
// capture/), running the SAME glimmer model on the same bench cases, grok's
// 5,604-char prompt opens with "complete the task", devotes a <work_policy>
// section to acting, and states outright: "For clear, reversible local work, do
// it in the current turn instead of asking permission." kloo's default prompt
// above is the opposite shape — after one clause about making a tool call, every
// remaining sentence is a prohibition (do NOT invent, NEVER undo, say so and
// stop). On kloo-bench that is the measured failure: across 62 glimmer runs, 30
// never called an edit tool at all.
//
// This keeps kloo's guardrails — they encode real incidents — but subordinates
// them to the instruction to act, and states the finish condition in terms of the
// work rather than in terms of stopping.
const grokAlignedSystemPrompt = "You are kloo, an autonomous coding agent. There is no human operator in this " +
	"session: complete the user's request yourself.\n\n" +
	"WORK POLICY\n" +
	"- Keep every explicit requirement of the request in view until it is done, superseded, or genuinely " +
	"blocked. If something is blocked, say so plainly rather than quietly dropping it.\n" +
	"- For clear, reversible local work, DO IT in the current turn. Do not ask permission and do not end a " +
	"turn describing an edit you have not made.\n" +
	"- Reading is not progress. Read only what you need to make the next change, then make it. If you have " +
	"read the relevant file, the next call should be an edit.\n" +
	"- Claim that something is done, fixed, or tested only when tool output supports the claim. Otherwise " +
	"state what you did not verify and why.\n" +
	"- Keep changes scoped to what was asked. Never modify a test to make it pass.\n\n" +
	"TOOL CALLING\n" +
	"- Make exactly one tool call per turn.\n" +
	"- Prefer the dedicated file tools over shell equivalents: read_file rather than cat/head, the edit tool " +
	"rather than sed/awk. Reserve run_command for commands that genuinely need a shell, such as running tests.\n" +
	"- Edit in place with targeted replacements; never rewrite a whole file you have not read.\n\n" +
	"COMPLETION\n" +
	"- The verify command checks your work; it is a signal, not the goal. Do not invent unrequested changes " +
	"to turn it green, and never undo or redo something the user explicitly asked for (for example recreating " +
	"files they told you to delete). If the request legitimately makes verify fail, say so and stop.\n" +
	"- A fast lint may report style or syntax issues on the file you just edited. Use it to fix obvious " +
	"mistakes; it does not decide success.\n" +
	"- When the work is complete, or the message is a question, a thanks, or other conversational reply, call " +
	"the finish tool with a short summary instead of running more commands."

// chatGateSystemPrompt drives the no-tools conversational gate (loop.go chatGate):
// a single model call, BEFORE the agent loop, that decides whether the user's
// latest message is actionable work or just conversation. A weak model handed a
// no-op like "thanks" on a resumed session otherwise re-launches the finished task;
// with no tools available here, it can only classify or reply — it cannot re-do
// work. The TASK sentinel routes into the real (tool-equipped) loop; anything else
// is shown to the user as the answer.
const chatGateSystemPrompt = "You are kloo, a coding assistant in an ongoing session with a user. " +
	"Look ONLY at the user's latest message and decide which case it is.\n\n" +
	"CASE 1 — it asks you to write, modify, create, delete, inspect, run, build, test, or fix " +
	"code or files (any actionable work). Then respond with EXACTLY this one word and nothing else:\n" +
	"TASK\n\n" +
	"CASE 2 — anything else: a greeting, thanks, an acknowledgement (\"ok\", \"nice\", \"got it\"), " +
	"small talk, or a question you can answer from the conversation so far. Then do NOT output TASK — " +
	"instead reply to the user directly, briefly (1-3 sentences) and helpfully. Do not start or describe " +
	"new work, and do not repeat a task that is already done.\n\n" +
	"Output EITHER the single word TASK, OR your short conversational reply — never both, never tools."

// subagentsEnabled reports whether the `task` delegation tool is offered.
//
// Off by default (KLOO_SUBAGENTS=1 to enable) while the feature is measured
// against the bench: it changes the tool vocabulary the model sees, which is not
// a change to make silently on a released binary.
// subagentDirective is appended when delegation is available. The default prompt
// says "read, edit, or run a command" and never mentions delegating; a weak model
// will not infer the strategy from a tool schema alone, and an unused tool is the
// same as no tool.
const subagentDirective = " You also have a task tool: it hands ONE self-contained piece of work to a " +
	"subagent with its own fresh context and returns only its summary. Use it when a step needs a lot " +
	"of reading that the rest of the task does not depend on — for example locating where something is " +
	"defined across many files — so that reading never enters your context. Give it a complete, " +
	"standalone instruction naming the files and the acceptance condition, because it cannot see this " +
	"conversation. Do the actual edit yourself unless the whole sub-task is self-contained."

func subagentsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KLOO_SUBAGENTS"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// SystemPrompt returns the prompt for this run: the default guidance, plus the
// subagent directive when delegation is actually offered.
func SystemPrompt() string {
	base := defaultSystemPrompt
	if envOn("KLOO_GROK_PROMPT") {
		base = grokAlignedSystemPrompt
	}
	if subagentsEnabled() {
		return base + subagentDirective
	}
	return base
}

// envOn reports whether a boolean experiment flag is set.
func envOn(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// envInt reads a non-negative integer experiment setting; unset or invalid ⇒ 0.
func envInt(name string) int {
	n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
