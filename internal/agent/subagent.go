package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/tools"
)

// NameTask is the subagent tool: the parent delegates a self-contained piece of
// work to a fresh agent loop and gets back only that agent's summary.
const NameTask = "task"

// Subagent defaults. Deliberately conservative: a delegated unit of work that
// needs more than this is not self-contained, and letting a child consume the
// parent's whole budget is the failure mode that makes delegation worse than
// doing the work inline.
const (
	// DefaultMaxSubagentDepth is 1: the top-level agent may delegate, and a
	// subagent may NOT delegate further. Unbounded nesting turns one bad plan
	// into an exponential fan-out against a paid endpoint.
	DefaultMaxSubagentDepth = 1
	// DefaultMaxSubagents caps how many children one run may spawn in total.
	DefaultMaxSubagents = 8
	// DefaultSubagentStepFrac is the child's step ceiling as a fraction of the
	// parent's configured MaxSteps.
	DefaultSubagentStepFrac = 0.5
)

// ErrSubagentDepth is returned when a subagent tries to spawn another.
var ErrSubagentDepth = errors.New("task: subagents may not spawn further subagents")

// ErrSubagentBudget is returned when a run has spawned its allowance.
var ErrSubagentBudget = errors.New("task: this run has reached its subagent limit")

// subagentSystemPrompt drives a delegated child.
//
// It differs from the top-level prompt in two ways that matter. First, the child
// has NO verifier: the parent owns the success gate, so a child that "makes the
// tests pass" by weakening them would be invisible. It is told to report, not to
// declare success. Second, it is told its context is its own — the parent's
// conversation is not visible to it, which is the entire point of delegating
// (the parent's window does not grow by the size of the child's exploration).
const subagentSystemPrompt = "You are a kloo subagent working on ONE self-contained piece of a larger task. " +
	"Each turn, make exactly one tool call. Use SEARCH/REPLACE edits; never rewrite whole files. " +
	"You cannot see the parent agent's conversation and you cannot delegate further. " +
	"When you are done, call the finish tool with a SUMMARY of what you found or changed: " +
	"name the files you touched and state plainly what is still unresolved. " +
	"Your summary is the only thing the parent will see, so a wrong or vague summary is worse " +
	"than saying you could not complete the work. Do not claim a change you did not make."

// taskTool lets a loop delegate a self-contained unit of work to a child loop.
//
// Why this exists: measured on kloo-bench, kloo scores 10/21 on glimmer where
// grok scores 19/21 — while spending the SAME total wall clock (10,914s vs
// 11,485s across 21 cases) and being marginally faster on the cases both solve.
// Eight harness-level explanations were tested and excluded (the exploration
// rail, memory eviction, edit friction, turn structure, redundant reads, prompt
// economy, and injecting the failing test at two different points). What remains
// is how the work is decomposed: grok delegates through a task tool, kloo runs a
// single linear loop.
type taskTool struct {
	parent *Loop
	depth  int
	spawns *int // shared across the run, so the cap counts TOTAL children
}

func (t taskTool) Name() string { return NameTask }

func (t taskTool) Description() string {
	return "Delegate ONE self-contained piece of work to a subagent with its own fresh context, " +
		"and get back only its summary. Use this when a step needs a lot of reading that the rest " +
		"of the task does not depend on — the subagent's exploration never enters your context. " +
		"Give it a complete, standalone instruction: it cannot see this conversation. " +
		"It cannot delegate further, and it does not decide whether the overall task succeeded."
}

func (t taskTool) Schema() tools.ParamSchema {
	return tools.ParamSchema{
		Properties: map[string]tools.Property{
			"instruction": {
				Type: "string",
				Description: "A complete, standalone instruction for the subagent. It cannot see " +
					"this conversation, so name the files, symbols and acceptance condition explicitly.",
			},
		},
		Required: []string{"instruction"},
	}
}

func (t taskTool) Invoke(ctx context.Context, c tools.Call) (tools.Result, error) {
	if t.depth >= t.parent.maxSubagentDepth() {
		return tools.Result{}, ErrSubagentDepth
	}
	if t.spawns != nil && *t.spawns >= t.parent.maxSubagents() {
		return tools.Result{}, ErrSubagentBudget
	}
	instruction := strings.TrimSpace(str(c.Args["instruction"]))
	if instruction == "" {
		return tools.Result{}, fmt.Errorf("%w: task needs a non-empty instruction", tools.ErrInvalidArgs)
	}
	if t.spawns != nil {
		*t.spawns++
	}
	res, _, err := t.parent.runSubagent(ctx, instruction, t.depth+1)
	return res, err
}

func (l *Loop) maxHandoffs() int {
	if l.MaxHandoffs > 0 {
		return l.MaxHandoffs
	}
	return 1
}

func (l *Loop) maxSubagentDepth() int {
	if l.SubagentDepth > 0 {
		return l.SubagentDepth
	}
	return DefaultMaxSubagentDepth
}

func (l *Loop) maxSubagents() int {
	if l.SubagentLimit > 0 {
		return l.SubagentLimit
	}
	return DefaultMaxSubagents
}

// runSubagent builds and runs a child loop, returning ONLY its summary.
//
// The child shares the parent's client, adapter, tool registry and workspace, so
// it edits the same repo — delegation is about context isolation, not sandboxing.
// What it does NOT share:
//
//   - the conversation (the point: the child's reads never enter the parent's window)
//   - the verifier (the parent owns the success gate; a child cannot declare the
//     task done, and cannot make a run "succeed" by weakening a test)
//   - the budget (its own ceiling, so one child cannot consume the whole run)
//   - working memory and churn state (fresh, or the parent's compaction history
//     would be attributed to the child's turns)
func (l *Loop) runSubagent(ctx context.Context, instruction string, depth int) (tools.Result, int, error) {
	child := *l // copy configuration, then override everything that must not be shared
	child.Verifier = nil
	child.Memory = nil
	child.Checkpoint = nil
	child.System = subagentSystemPrompt
	child.ChatSystem = "" // no conversational gate: this is always work, never chat
	child.Churn = NewChurnDetector(config.DefaultChurnRounds)
	child.Budget = l.subagentBudget()
	child.Registry = l.registryForDepth(depth)
	// Model routing: a delegated subtask may run on a different model than the
	// parent. The child gets its own client so the parent's endpoint, key and
	// model stay untouched.
	if m := strings.TrimSpace(l.SubagentModel); m != "" && l.NewSubagentClient != nil {
		ep := strings.TrimSpace(l.SubagentEndpoint)
		if ep == "" {
			ep = l.Endpoint
		}
		if c := l.NewSubagentClient(ep, m); c != nil {
			child.Model = m
			child.Endpoint = ep
			child.Client = c
		}
	}
	child.subagentDepth = depth
	child.subagentSpawns = l.subagentSpawns // the cap is run-wide, not per level

	// The child's own callbacks must not drive the parent's UI/state machine: a
	// nested StateVerify or a nested OnTool would be indistinguishable from the
	// parent's own progress in the TUI and in the benchmark counters.
	child.OnState = nil
	child.OnTool = nil
	child.OnDelta = nil
	child.OnProgress = nil
	child.OnBeforeEdit = l.OnBeforeEdit // approval still applies: a child may not bypass it
	// A delegated child must never delegate again. It copies the parent's config,
	// including AutoDelegate and DelegateAfterReads, and the depth limit only
	// guarded task-tool REGISTRATION — so a child reading past the threshold spawned
	// a grandchild, which could spawn another. Found on kloo-bench C17: three
	// subagent completions (19, 21, 25 steps) in a run meant to delegate once,
	// finishing 11s under the ceiling. canAutoDelegate also checks depth, so this is
	// belt and braces rather than the only guard.
	child.AutoDelegate = false

	rep, err := child.Run(ctx, instruction)
	if err != nil {
		return tools.Result{}, 0, fmt.Errorf("task: subagent failed: %w", err)
	}
	if l.OnSubagent != nil {
		l.OnSubagent(rep.Steps, rep.Reason, rep.Err)
	}
	return tools.Result{Output: subagentReport(rep)}, rep.Steps, nil
}

// subagentBudget gives the child its own ceiling, derived from the parent's.
func (l *Loop) subagentBudget() Budget {
	steps := 0
	if l.Budget != nil {
		steps = l.Budget.Stats().MaxSteps
	}
	if steps <= 0 {
		steps = 30
	}
	n := int(float64(steps) * DefaultSubagentStepFrac)
	// An explicit cap wins. Measured on kloo-bench, the default (half of a 60-step
	// parent = 30) let delegated cases run to 2381s and 2400s against a 2400s
	// ceiling: a pass 19 seconds from the wire is a coin flip, and the time spent
	// also desynchronised a paired control/experiment run by 3x.
	if l.SubagentMaxSteps > 0 {
		n = l.SubagentMaxSteps
	}
	if n < 4 {
		n = 4
	}
	return &stepBudget{maxSteps: n}
}

// registryForDepth returns the tool set a loop at this depth may use. At the
// depth limit the task tool is omitted entirely rather than left present and
// erroring: a tool the model can see but never use invites repeated failed calls.
func (l *Loop) registryForDepth(depth int) *tools.Registry {
	if depth >= l.maxSubagentDepth() {
		return tools.Without(l.Registry, NameTask)
	}
	return l.Registry
}

// subagentReport renders what the parent sees. Only the summary and the files
// touched — never the child's transcript, which is the whole reason to delegate.
func subagentReport(rep *Report) string {
	var b strings.Builder
	b.WriteString("subagent finished (")
	b.WriteString(string(rep.Reason))
	b.WriteString(")\n")
	if s := strings.TrimSpace(rep.Summary); s != "" {
		b.WriteString("summary: ")
		b.WriteString(s)
		b.WriteString("\n")
	}
	if rep.Reason != ReasonSuccess && rep.Reason != ReasonUnverified && rep.Reason != ReasonAnswered {
		b.WriteString("NOTE: the subagent did not finish cleanly — treat its summary as partial " +
			"and verify anything you depend on.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// stepBudget is the child's ceiling: steps only. Tokens and wall-clock stay the
// parent's problem — the parent's own budget keeps counting while the child runs,
// so a slow child still trips the run-level ceilings.
type stepBudget struct {
	steps    int
	maxSteps int
	tokens   int
}

func (b *stepBudget) Observe(step int) { b.steps = step }
func (b *stepBudget) AddTokens(n int)  { b.tokens += n }
func (b *stepBudget) Check() (bool, BudgetKind) {
	if b.maxSteps > 0 && b.steps >= b.maxSteps {
		return true, BudgetSteps
	}
	return false, ""
}
func (b *stepBudget) Stats() BudgetStats {
	return BudgetStats{Steps: b.steps, MaxSteps: b.maxSteps, Tokens: b.tokens}
}
func (b *stepBudget) Reset() { b.steps, b.tokens = 0, 0 }

// autoDelegateCorrective is what the parent sees after kloo delegates FOR it.
func autoDelegateCorrective(report string) llm.Message {
	return llm.Message{Role: llm.RoleUser, Content: "You were reading without making a change, so a " +
		"subagent has done the work for you. Its report:\n\n" + report +
		"\n\nThe edits it describes are ALREADY APPLIED to the working tree. Do not redo them and do " +
		"not re-read those files. Verify the result with run_command, fix anything it got wrong, " +
		"and call finish when the task is done."}
}

// autoDelegateInstruction is the standalone brief handed to the delegate. It must
// be self-contained: the child cannot see the parent's conversation.
//
// It delegates the WORK, not an investigation. Measured on kloo-bench, an
// investigate-only brief fired correctly, returned a report, and changed nothing:
// the parent read for 34 more steps and never acted. That is the third time
// injected information has been ignored by this model (the failing test at the
// explore nudge, the same test before the run started, and now a specific
// investigation report). Injecting context does not move it.
//
// What DOES work is a fresh context with a focused instruction: the investigator
// child completed its own task and returned a usable report while the parent was
// re-reading one file 12 times in a row. So the child is given the edit to make.
func autoDelegateInstruction(task string) string {
	return "Make this change yourself. Read only what you need, then EDIT the source.\n\n" +
		task + "\n\n" +
		"Rules: do NOT modify any test file — the tests define the expected behaviour and are " +
		"already correct. Change the SOURCE so the tests pass. When you are done, call finish with " +
		"a summary naming every file you edited and what you changed in each. " +
		"If you could not make the change, say so plainly and name what blocked you — " +
		"a wrong summary is worse than an honest failure."
}

// autoDelegate runs the delegate and returns its report for injection.
//
// Why kloo delegates instead of letting the model choose: measured on
// kloo-bench, glimmer is OFFERED the task tool (it appears in its own tool list)
// and never calls it — 0 delegations across 36 calls on a case it then lost to
// explore-stop. That matches every other result on this model: it does not change
// strategy when offered a better one, or when told to. So the harness has to
// drive the decomposition rather than suggest it.
func (l *Loop) autoDelegate(ctx context.Context, task string) (llm.Message, int, bool) {
	res, steps, err := l.runSubagent(ctx, autoDelegateInstruction(task), 1)
	if err != nil || strings.TrimSpace(res.Output) == "" {
		return llm.Message{}, steps, false
	}
	return autoDelegateCorrective(res.Output), steps, true
}

// delegateUntilEdit widens auto-delegation from "nothing acted on yet" to "nothing
// edited yet" (KLOO_DELEGATE_UNTIL_EDIT=1). Off by default so the measured
// behaviour of KLOO_SUBAGENTS is unchanged.
func delegateUntilEdit() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KLOO_DELEGATE_UNTIL_EDIT"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// delegationBlocked decides whether auto-delegation may still fire.
//
// Default: blocked once the model has taken ANY action. With untilEdit: blocked
// only once it has EDITED. Measured on kloo-bench C66, the default is too strict —
// the model ran the failing test early (a healthy move), which disabled delegation
// for the rest of the run, and it then spun 35 steps.
func delegationBlocked(everActed, edited, untilEdit bool) bool {
	if untilEdit {
		return edited
	}
	return everActed
}

// canAutoDelegate reports whether THIS loop may start a harness-initiated handoff:
// the feature is on, and the loop is above the depth limit.
func (l *Loop) canAutoDelegate() bool {
	return (l.EnableSubagents || l.AutoDelegate) && l.subagentDepth < l.maxSubagentDepth()
}

// delegateOnStop enables the rescue handoff at the explore rail
// (KLOO_DELEGATE_ON_STOP=1). Off by default.
func delegateOnStop() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KLOO_DELEGATE_ON_STOP"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// mapDeprioritiseTests pushes test files down the repo map
// (KLOO_MAP_SKIP_TESTS=1). Off by default.
func mapDeprioritiseTests() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KLOO_MAP_SKIP_TESTS"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

var _ = time.Second

// noRepoMap drops the repo-map section from every prompt (KLOO_NO_MAP=1). Off by
// default: the map is a core part of how kloo assembles context, and removing it
// is a structural change that earns the default path only by measurement.
func noRepoMap() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KLOO_NO_MAP"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
