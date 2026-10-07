package agent

import (
	"context"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/repomap"
	"github.com/lokalhub/kloo/internal/tools"
)

// ── The pre-edit verify baseline, and the reach of the edit scope ─────────────
//
// THE INCIDENT (2026-10-06, an Ionic/Angular app). A run was asked to "make the
// game board mobile friendly at <=480px" and scoped with
// `--allow 'frontend/src/app/pages/game/**'`. The model produced a genuinely
// correct SCSS patch. kloo scored the run success:false and then drove the model
// off-task until it ran out of budget. The chain:
//
//  1. The project's `npm test` was ALREADY RED before the run started — one
//     committed-red spec, frontend/src/app/pages/login/login.page.spec.ts:53
//     ("constrains the login form width"). 43 passed, 1 failed, and that 1 has
//     nothing to do with the game board.
//  2. kloo's verify gate saw red, so the run could not be a success, and
//     editCorrective told the model: "Reading more will not complete this task:
//     the code must change for the failing test to pass. This turn, make your
//     best edit… Do not read, do not search, do not run a command, do not call
//     finish."
//  3. The only failing test was the login one. So the model obeyed and edited
//     login.page.ts — which the --allow scope DENIED. It had nowhere legal to go:
//     21 consecutive denied edits to one file, 1,553,679 tokens, all off-task.
//
// kloo converted a pre-existing, out-of-scope test failure into a mandatory,
// impossible, scope-violating instruction. Two defects, fixed together here
// because neither fix works alone:
//
//   - kloo could not tell "this test was already failing before I touched
//     anything" from "my edit broke this". Hence the BASELINE: one verify run
//     taken before the agent's first edit, whose failing set every later
//     judgement and corrective is measured against.
//   - kloo knew the write scope only at write time, so the corrective could order
//     an edit the policy would reject and then punish the model for obeying.
//     Hence REACH: the scope is consulted BEFORE a corrective is composed.
//
// Both are deliberately inert on an UNSCOPED run (see verifyBaselineMode): with
// no scope there is no such thing as an unreachable file, and every kloo-bench
// case is red at step 0 by construction — tolerating baseline-red there would
// make every case pass vacuously. The one configuration where kloo can be ordered
// to do the impossible is the one configuration this changes.

// VerifyBaseline is the verify signal recorded BEFORE the agent's first edit: the
// state of the project as kloo found it.
//
// Taken is the only field callers may trust without checking the rest: a baseline
// that could not be established (no verifier, command missing, timeout, setup
// error) leaves Taken false, and every consumer below then behaves exactly as kloo
// did before this file existed. FAILING OPEN is not a nicety here — a baseline is
// an optimisation on the judgement, never a precondition for running.
type VerifyBaseline struct {
	// Taken is true only for a baseline that RAN and produced a usable signal.
	Taken bool
	// Passed is the baseline verify's own verdict. A green baseline is still worth
	// recording: it proves any later red is NEW, which is the one case where
	// "your change broke this" is a true statement.
	Passed bool
	// Failing are the failing test identities at baseline, as failingAssertions
	// reads them out of the output. May be empty for a red baseline whose runner
	// kloo cannot parse — Key then carries the identity instead.
	Failing []string
	// Key is normalizeChurn of the whole baseline failing output: the fallback
	// identity for a runner whose per-test names kloo cannot extract. Two reds with
	// the same Key are the same red, volatile bits (durations, temp paths) aside.
	Key string
	// FailedStage is which LAYER produced the baseline's non-success outcome
	// ("precheck"/"postcheck"/"" for the verify command itself). Part of the identity:
	// a LayeredVerifier precheck failure reports the HOOK's command and output, and
	// comparing a precheck's red to a verify's red would be comparing two different
	// commands (VerifyResult.FailedStage, types.go).
	FailedStage string
	// BuildBroken records that the baseline output ALREADY matched the build-break
	// signatures, so a later match must not be reported to the model as "YOUR LAST
	// EDIT BROKE THE BUILD" — it did not.
	BuildBroken bool
	// Unreachable are the files the baseline's failing output implicates that the
	// scope policy will NOT let the model write (see failingTestFiles / scopeDenies).
	// Non-empty only when every implicated file is denied, which is what makes the
	// failure out of reach rather than merely inconvenient.
	Unreachable []string
	// Skipped records WHY no baseline was taken, for the run log. Empty when Taken.
	Skipped string
}

// verifyBaselineMode is the tri-state switch for the baseline, following kloo's
// sign convention: 0 (or unset) is the built-in COMPUTED default, a negative value
// disables, a positive value forces on.
//
//	KLOO_VERIFY_BASELINE unset/0  → AUTO: on when a scope policy restricts writes
//	KLOO_VERIFY_BASELINE=1        → always take a baseline
//	KLOO_VERIFY_BASELINE=-1 (off) → never (pre-baseline behaviour, byte-identical)
//
// AUTO rather than ALWAYS is a deliberate, measured choice and the comment is the
// place to say why: every kloo-bench case ships with its target test already
// failing, so a baseline that excused pre-existing red on an unscoped run would
// score the entire benchmark as a vacuous pass. A scope policy is the signal that
// some of the red is not the agent's to fix.
func verifyBaselineMode() int { return envTri("KLOO_VERIFY_BASELINE") }

// envTri parses a knob whose default is COMPUTED rather than a constant: 0 (or
// unset, or unparseable) means "use the built-in default", negative means
// disabled, positive means forced on. The word forms are accepted too, because a
// human reaching for this in the field will type `=off` before `=-1`.
func envTri(name string) int {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	switch v {
	case "":
		return 0
	case "off", "false", "no", "never", "disable", "disabled":
		return -1
	case "on", "true", "yes", "always":
		return 1
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0 // unreadable ⇒ the default, never a silent disable
	}
	return n
}

// baselineWanted reports whether THIS run takes a pre-edit verify baseline.
func (l *Loop) baselineWanted() bool {
	if l.Verifier == nil {
		return false // nothing to run; unverified mode cannot have a baseline
	}
	switch m := verifyBaselineMode(); {
	case m < 0:
		return false
	case m > 0:
		return true
	default:
		return l.Scope.Active() // AUTO — see verifyBaselineMode
	}
}

// takeVerifyBaseline runs the configured verifier ONCE against the tree as kloo
// found it and records what was already red.
//
// Called from the first-edit hook rather than at step 0 on purpose: a run that
// only reads (a question, a review) never pays for a verify it has no use for,
// and the tree at the moment before the first edit is still exactly the tree kloo
// was handed. The one thing it cannot see is a model run_command that mutated the
// tree first — which cannot happen in the configuration that engages this, since
// an active scope policy withholds the model-facing shell (tools.Workspace.
// ModelShellDisabled).
//
// Every failure path leaves Taken false with a reason in Skipped. That is the
// fail-open contract: a baseline that cannot be taken must change nothing.
func (l *Loop) takeVerifyBaseline(ctx context.Context) VerifyBaseline {
	if l.Verifier == nil {
		return VerifyBaseline{Skipped: "no verifier"}
	}
	v := l.Verifier.Verify(ctx)
	switch {
	case v.Err != nil:
		// Non-runnable, timed out, jail escape. NOT an error for the run: the
		// baseline is simply unavailable and the loop carries on as before.
		return VerifyBaseline{Skipped: "verify did not run: " + v.Err.Error()}
	case v.ExitCode == 126 || v.ExitCode == 127:
		// The shell ran but the RUNNER never did (127 = not found, 126 = not
		// executable). That is a setup error, not a test result — the same
		// distinction lint.go already makes — and a setup error says nothing about
		// which tests were failing, so it must never become a baseline.
		return VerifyBaseline{Skipped: "verify command not runnable (exit " + strconv.Itoa(v.ExitCode) + ")"}
	}

	out := failingOutput(v)
	b := VerifyBaseline{Taken: true, Passed: v.Passed}
	if v.Passed {
		return b
	}
	b.Failing = failingAssertions(out)
	b.Key = normalizeChurn(out)
	b.FailedStage = v.FailedStage
	b.BuildBroken, _ = buildBreak(out)
	if files := l.failingTestFiles(out); len(files) > 0 {
		if denied := l.scopeDenies(files); len(denied) == len(files) {
			b.Unreachable = denied
		}
	}
	return b
}

// newFailures returns the failing identities in cur that the baseline did NOT
// already have — the only failures a corrective may honestly attribute to the
// agent's own changes. With no usable baseline it returns cur unchanged, which is
// the pre-baseline behaviour.
func (b VerifyBaseline) newFailures(cur []string) []string {
	if !b.Taken || b.Passed {
		return cur
	}
	was := make(map[string]bool, len(b.Failing))
	for _, f := range b.Failing {
		was[f] = true
	}
	var out []string
	for _, f := range cur {
		if !was[f] {
			out = append(out, f)
		}
	}
	return out
}

// retire drops from the excused set every baseline failure that is no longer
// failing, permanently.
//
// Without it a FLAKY red at baseline would excuse a real regression for the rest of
// the run: test A fails at baseline by luck, passes on the next verify, then fails
// again for a reason the agent introduced — and "it was red at baseline" would wave
// it through. Once a baseline-red test has been seen passing it never re-enters the
// excused set.
//
// cur is the currently-failing identities; passed says the whole verify went green.
// When the verify is still red but cur is EMPTY, kloo could not read per-test
// identities out of that runner's output and therefore knows nothing about which
// tests now pass — so it retires nothing rather than guessing.
func (b *VerifyBaseline) retire(cur []string, passed bool) {
	if !b.Taken || len(b.Failing) == 0 {
		return
	}
	if passed {
		b.Failing = nil // everything the baseline listed is demonstrably fixed
		return
	}
	if len(cur) == 0 {
		return
	}
	still := make(map[string]bool, len(cur))
	for _, f := range cur {
		still[f] = true
	}
	kept := make([]string, 0, len(b.Failing))
	for _, f := range b.Failing {
		if still[f] {
			kept = append(kept, f)
		}
	}
	b.Failing = kept
}

// unchangedSince reports whether v's red is the SAME red the baseline recorded —
// no failing test the baseline did not already have.
//
// Identity is per-test when kloo can read the runner's output, and the normalised
// whole-output signature when it cannot. The fallback is the conservative one: a
// run that broke something new almost always changes the output, so an unparseable
// runner degrades toward "this is new", never toward a false all-clear.
func (b VerifyBaseline) unchangedSince(v VerifyResult) bool {
	// v.Command == "" is a verify that never ran (the zero value the loop carries
	// before its first verify). It is not "the same red" — it is no signal at all,
	// and treating it as agreement would hand out a success for an unverified tree.
	if !b.Taken || b.Passed || v.Command == "" || v.Err != nil || v.Passed {
		return false
	}
	if v.FailedStage != b.FailedStage {
		return false // a precheck's red and a verify's red are not the same red
	}
	out := failingOutput(v)
	if cur := failingAssertions(out); len(cur) > 0 || len(b.Failing) > 0 {
		return len(b.newFailures(cur)) == 0
	}
	return b.Key != "" && normalizeChurn(out) == b.Key
}

// outOfReach reports whether the red kloo is looking at is red the agent is not
// permitted to fix: unchanged since the baseline, AND every file the failing
// output implicates is denied by the scope policy.
//
// BOTH halves are required. "Unchanged since baseline" alone would excuse a run
// that broke an out-of-scope file through some other route — damage is never
// excused. "Out of scope" alone would excuse damage the run itself caused inside
// a file it was never allowed to touch.
func (l *Loop) outOfReach(b VerifyBaseline, v VerifyResult) bool {
	return len(b.Unreachable) > 0 && b.unchangedSince(v)
}

// outOfReachForCorrective answers the question a CORRECTIVE has to answer, which is
// weaker than the one the success gate asks: is the red kloo is about to point the
// model at red the model is not permitted to fix?
//
// It differs in one case, and that case is the incident's. kloo nudges a reading
// model long before the first edit, so there is often no verify result newer than
// the baseline — the baseline IS the current signal. outOfReach refuses to call
// that "unchanged" on purpose (a run must never be called a success on a verify
// that never ran), but a corrective is not a verdict: it only has to avoid
// ordering an impossible edit, and the baseline alone is enough to know the order
// would be impossible.
func (l *Loop) outOfReachForCorrective(b VerifyBaseline, v VerifyResult) bool {
	if len(b.Unreachable) == 0 {
		return false
	}
	if v.Command == "" {
		return b.Taken && !b.Passed // nothing has verified since; the baseline stands
	}
	return b.unchangedSince(v)
}

// scopeDenies returns the subset of files this run's scope policy will NOT let the
// model write. An inactive (or nil) policy denies nothing, so it returns nil and
// every caller falls back to its pre-scope behaviour.
func (l *Loop) scopeDenies(files []string) []string {
	if !l.Scope.Active() {
		return nil
	}
	var denied []string
	for _, f := range files {
		if !l.Scope.CanWrite(f).Allowed {
			denied = append(denied, f)
		}
	}
	return denied
}

// failingTestFiles returns the workspace files a failing verify output implicates:
// the test files it names, plus (for each) the source files that test imports.
//
// Every candidate must resolve to a file that really exists in the workspace, so
// a stray word that happens to look like a path cannot widen or narrow the set.
// When nothing resolves the result is empty and the callers fall back to today's
// behaviour — the honest outcome for a runner whose output names no paths at all.
//
// Why the imports matter: a test whose own file is out of scope may still be
// fixable through the source it covers, and ordering an edit is legitimate then.
// The incident's login spec fails that test — both the spec and login.page.ts /
// login.page.scss sit outside `frontend/src/app/pages/game/**` — which is exactly
// what makes it unreachable rather than merely awkward.
func (l *Loop) failingTestFiles(out string) []string {
	if strings.TrimSpace(out) == "" || l.Root == "" {
		return nil
	}
	ws, err := tools.NewWorkspace(l.Root)
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var files []string
	add := func(rel string) {
		if rel == "" || seen[rel] {
			return
		}
		seen[rel] = true
		files = append(files, rel)
	}
	for _, cand := range pathLikeTokens(out) {
		rel, ok := l.resolveWorkspacePath(ws, cand)
		if !ok || !repomap.IsTestPath(rel) {
			continue
		}
		add(rel)
		for _, imp := range l.importsOf(rel) {
			add(imp)
		}
		if len(files) >= maxFailingTestFiles {
			break
		}
	}
	return files
}

// maxFailingTestFiles bounds the implicated set. A stack trace can name dozens of
// frames; the decision only needs enough of them to be confident, and an unbounded
// walk over a huge output is a cost with no payoff.
const maxFailingTestFiles = 24

// pathLikeTokens pulls path-shaped substrings out of arbitrary runner output:
// a vitest "FAIL src/x.spec.ts", a karma/jasmine stack frame
// "at … frontend/src/app/pages/login/login.page.spec.ts:53:5", a go test
// "internal/agent/loop_test.go:12". Line/column suffixes and the usual URL-ish
// prefixes are stripped; whether the result is a real file is decided by
// resolveWorkspacePath, not by the regexp.
func pathLikeTokens(out string) []string {
	var toks []string
	seen := map[string]bool{}
	for _, m := range pathLikePattern.FindAllString(out, -1) {
		m = strings.TrimPrefix(m, "webpack:///")
		m = strings.TrimPrefix(m, "webpack://")
		m = strings.TrimPrefix(m, "file://")
		m = strings.TrimLeft(m, "./")
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		if toks = append(toks, m); len(toks) >= maxPathLikeTokens {
			break
		}
	}
	return toks
}

// maxPathLikeTokens bounds the scan of one verify output. Verify output is already
// clipped by the run_command tool, but a long karma trace still holds hundreds of
// frames pointing at the same handful of files.
const maxPathLikeTokens = 200

// pathLikePattern matches a slash-or-dot path ending in a source extension,
// optionally followed by :line[:col]. Anchored on the extension rather than on a
// leading slash, because every runner prints these differently.
var pathLikePattern = regexp.MustCompile(
	`[A-Za-z0-9_@./\\:-]*[A-Za-z0-9_@-]\.(?:spec\.[tj]sx?|test\.[tj]sx?|[tj]sx?|mjs|cjs|go|py|rb|java|kt|swift|rs|php|scss|css|html|vue|svelte)`)

// resolveWorkspacePath maps a token from runner output onto a real
// workspace-relative file, or reports that it is not one.
//
// Three attempts, cheapest first: the token as given; the token with leading path
// segments peeled off (a runner that prints an absolute or bundler-prefixed path);
// and finally a SUFFIX match against the walked repo, for the common case where the
// runner's working directory is a subdirectory of the workspace root and its paths
// are therefore shorter than kloo's. The walk is the same bounded, gitignore-aware
// one the repo map uses and is memoised per run, so this costs one walk at most.
func (l *Loop) resolveWorkspacePath(ws tools.Workspace, tok string) (string, bool) {
	tok = strings.Trim(tok, "()[]<>'\",;")
	// Strip a :line[:col] suffix. Done by hand rather than with a regexp group so a
	// Windows drive letter ("C:\…") is not mistaken for one.
	for i := 0; i < 2; i++ {
		if j := strings.LastIndex(tok, ":"); j > 1 {
			if _, err := strconv.Atoi(tok[j+1:]); err == nil {
				tok = tok[:j]
				continue
			}
		}
		break
	}
	tok = tools.NormalizeScopePath(tok)
	if tok == "" {
		return "", false
	}
	exists := func(rel string) bool {
		_, err := tools.ReadFile(ws, rel)
		return err == nil
	}
	if exists(tok) {
		return tok, true
	}
	// Peel leading segments: handles an absolute path, or a bundler prefix the
	// trimming above did not know about.
	for rest := tok; strings.Contains(rest, "/"); {
		rest = rest[strings.Index(rest, "/")+1:]
		if rest != "" && exists(rest) {
			return rest, true
		}
	}
	// Suffix match against the repo. Only accepted when it is UNAMBIGUOUS: two
	// files ending in the same relative path tell us nothing about which one failed,
	// and guessing would hand a wrong answer to a scope decision.
	var hit string
	for _, rel := range l.walkedFiles() {
		if rel == tok || strings.HasSuffix(rel, "/"+tok) {
			if hit != "" {
				return "", false // ambiguous
			}
			hit = rel
		}
	}
	return hit, hit != ""
}

// walkedFiles is the run's memoised list of workspace-relative file paths, from
// the same walk the repo map uses (gitignore-aware, build output hard-skipped).
// Memoised because the only caller may be asked about many tokens from one output
// and the answer cannot change mid-decision.
func (l *Loop) walkedFiles() []string {
	if l.walkedCache != nil {
		return l.walkedCache
	}
	l.walkedCache = []string{} // negative-cache: a failed walk is not retried per token
	if l.Root == "" {
		return l.walkedCache
	}
	nodes, err := repomap.Walk(l.Root)
	if err != nil {
		return l.walkedCache
	}
	out := make([]string, 0, len(nodes))
	for _, n := range repomap.Files(nodes) {
		out = append(out, n.Path)
	}
	l.walkedCache = out
	return l.walkedCache
}

// outOfScopeVerifyCorrective is the corrective for the incident's exact state: the
// verify is red, nothing the agent did made it red, and every file the failure
// implicates is one the scope policy forbids it to write.
//
// What it must NOT do is as important as what it says. It does not demand an edit
// (there is no legal edit that could help), it does not withhold the read tools
// (the caller leaves editOnlyLeft alone), and it does not leave the model guessing
// why its last attempt was denied. kloo spent 21 steps and 1.5M tokens learning
// that the alternative does not work.
func outOfScopeVerifyCorrective(unreachable, allowed []string) llm.Message {
	var b strings.Builder
	b.WriteString("About the failing verify: it was ALREADY FAILING before this run started, and it is NOT yours to fix. ")
	b.WriteString("The files that failure comes from are outside the edit scope you were given, so you are not permitted to change them:\n  - ")
	b.WriteString(strings.Join(clipList(unreachable, 8), "\n  - "))
	b.WriteString("\nDo not try to edit them — every attempt will be denied, and the denial is the policy working, not a mistake you made. ")
	b.WriteString("Nothing you do can turn this check green, and you are NOT being asked to.\n")
	if len(allowed) > 0 {
		b.WriteString("The ONLY files you may change this run are the ones matching:\n  - " +
			strings.Join(clipList(allowed, 8), "\n  - ") + "\n")
	}
	b.WriteString("Judge your own work on the task you were given, not on this check. ")
	b.WriteString("Read whatever you still need to read, finish the change the task asks for, and then call finish with a summary ")
	b.WriteString("saying what you changed and that the pre-existing failure above is untouched.")
	return llm.Message{Role: llm.RoleUser, Content: b.String()}
}

// outOfScopeVerifyNote is the SHORT form of the message above, for the pinned
// last-verify block (memory.go verifyPin) which is re-sent on every turn.
//
// Short on purpose. The pin is the most expensive text in the prompt — it recurs
// every turn for the rest of the run — and the long explanation belongs in the
// corrective that fires once. What the pin must carry is the one fact that stops the
// model reading this red as its job.
func outOfScopeVerifyNote(unreachable, allowed []string) string {
	var b strings.Builder
	b.WriteString("NOTE: this failure was already there before this run, and it comes from files you are NOT allowed to edit (")
	b.WriteString(strings.Join(clipList(unreachable, 3), ", "))
	b.WriteString("). It is not your task and you cannot make it green.")
	if len(allowed) > 0 {
		b.WriteString(" You may only change files matching: " + strings.Join(clipList(allowed, 3), ", ") + ".")
	}
	return b.String()
}

// clipList bounds a list for a model-facing message, saying how many were dropped
// rather than silently truncating (a list that ends mid-way reads as the whole set).
func clipList(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	out := append([]string(nil), items[:n]...)
	return append(out, "… and "+strconv.Itoa(len(items)-n)+" more")
}
