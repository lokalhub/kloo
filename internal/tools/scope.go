package tools

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// scopeWalkSkipDirs are the trees a scope walk never descends into. They hold
// generated and vendored content that no allow glob is meant to point at, and
// walking them is the difference between a millisecond and a second on a real
// checkout — this walk answers a rail decision, not a repo-map one.
var scopeWalkSkipDirs = map[string]bool{".git": true, "node_modules": true}

// ScopePolicy is the hard, model-facing file-scope boundary that sits INSIDE the
// workspace jail (workspace.go). The jail (Workspace.Resolve) is the outer
// path-safety primitive — it stops traversal outside the root; ScopePolicy is a
// second, narrower layer that constrains WHICH in-jail files the model may write.
//
// A weak local model can otherwise edit lockfiles, generated output, tests, or
// unrelated source that happen to be inside the jail and still make verify pass
// for the wrong reason (A1). The policy is consulted by WriteFile/EditFile
// (files.go) before any disk side effect, and — because a shell command can mutate
// any in-jail path without going through those tools — an active policy also
// disables the model-facing run_command (builtins.go / disabled_shell.go).
//
// Matching is on slash-normalized, workspace-relative paths. Patterns are globs
// with documented "**" (any path segments) plus per-segment "*"/"?"; see globMatch.
//
// root is the workspace the policy was built for ("" when unknown). It is set by
// Workspace.WithScope, which is the one place that holds both the policy and the
// tree it applies to — the policy is constructed from flags and a manifest with no
// filesystem in sight, and attaching it to the jail is where the two meet.
// AllowsAnyWrite needs the tree to answer its question at all.
type ScopePolicy struct {
	allow    []scopeGlob
	deny     []scopeGlob
	readOnly []scopeGlob
	root     string
}

// Decision-class strings for a scope denial. They are the stable
// failure_detail.class values surfaced in KLOO_RESULT_JSON (headless.go) and the
// A7 stop rules (agent/loop.go), so they are exported and must not drift.
const (
	// ScopeClassDeny — the path matched a deny glob (deny wins over allow).
	ScopeClassDeny = "deny"
	// ScopeClassOutsideAllow — a non-empty allow set is configured and the path
	// matched none of it.
	ScopeClassOutsideAllow = "outside_allow"
	// ScopeClassReadOnly — the path matched a read_only glob (A2): readable, but
	// not writable. read_only wins over allow.
	ScopeClassReadOnly = "read_only"
	// ScopeClassRunCommandDisabled — a model-facing run_command was rejected because
	// a scope policy is active (the shell could bypass the write checks).
	ScopeClassRunCommandDisabled = "run_command_disabled_for_scope"
)

// ErrOffScope is the sentinel every scope denial wraps, so the loop and the
// headless classifier can match it with errors.Is regardless of the class. The
// concrete *ScopeError (extracted with errors.As) carries the bounded detail.
var ErrOffScope = errors.New("tools: write blocked by file-scope policy")

// ScopeError is a scope denial with bounded, machine-readable detail. It wraps
// ErrOffScope (via Unwrap) so errors.Is(err, ErrOffScope) matches, and is
// extracted with errors.As for the class/tool/path/rule fields the JSON summary
// and the A7 stop rules report.
type ScopeError struct {
	Class   string // one of the ScopeClass* constants
	Tool    string // the model tool that was denied (edit_file/write_file/run_command)
	Path    string // the workspace-relative path (bounded; "" for run_command)
	Rule    string // the glob that matched (for deny/outside_allow/read_only)
	Message string // bounded human/model-facing message
}

func (e *ScopeError) Error() string { return e.Message }

// Unwrap ties every ScopeError to the ErrOffScope sentinel for errors.Is.
func (e *ScopeError) Unwrap() error { return ErrOffScope }

// ScopeDecision is the result of a write check: Allowed, or the denial class +
// the matching rule.
type ScopeDecision struct {
	Allowed bool
	Class   string // "" when Allowed; otherwise a ScopeClass* value
	Rule    string // the glob that matched (empty for a pure outside_allow with no single rule)
	Path    string // the normalized workspace-relative path checked
}

// scopeGlob is a compiled scope pattern: its original text (for messages) plus
// the anchored regexp it was translated to.
type scopeGlob struct {
	pattern string
	re      *regexp.Regexp
}

// NewScopePolicy builds a policy from allow/deny/read_only glob lists (already
// merged from CLI flags + .kloo/scope.yaml by the caller). Empty patterns are
// dropped and paths are normalized. It returns (nil, nil) when all three lists are
// empty — "no policy", so the workspace behaves exactly as before (the jail is the
// only boundary). A malformed glob returns an error.
func NewScopePolicy(allow, deny, readOnly []string) (*ScopePolicy, error) {
	ag, err := compileGlobs(allow)
	if err != nil {
		return nil, err
	}
	dg, err := compileGlobs(deny)
	if err != nil {
		return nil, err
	}
	rg, err := compileGlobs(readOnly)
	if err != nil {
		return nil, err
	}
	if len(ag) == 0 && len(dg) == 0 && len(rg) == 0 {
		return nil, nil
	}
	return &ScopePolicy{allow: ag, deny: dg, readOnly: rg}, nil
}

// Active reports whether the policy constrains anything (any allow/deny/read_only
// glob). A nil policy is never active.
func (p *ScopePolicy) Active() bool {
	return p != nil && (len(p.allow) > 0 || len(p.deny) > 0 || len(p.readOnly) > 0)
}

// AllowPatterns returns the allow globs as configured, for a message that has to
// tell the model what it MAY change. Added for the out-of-scope verify corrective
// (agent/baseline.go): a corrective that only says "that file is denied" leaves the
// model to guess where it is allowed to work, and in the incident that motivated it
// the model guessed wrong 21 times in a row.
//
// A nil/empty allow set returns nil — "everything in the jail except what deny and
// read_only remove", which is not expressible as a list of allow patterns.
func (p *ScopePolicy) AllowPatterns() []string {
	if p == nil || len(p.allow) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.allow))
	for _, g := range p.allow {
		out = append(out, g.pattern)
	}
	return out
}

// CanWrite evaluates whether tool may write relPath under this policy. Precedence
// is deterministic and documented (A2): deny, then read_only, then outside_allow.
// An empty allow set means "everything in the jail is allowed" unless deny/read_only
// narrows it. A nil policy allows everything (the caller then relies on the jail).
func (p *ScopePolicy) CanWrite(relPath string) ScopeDecision {
	rel := NormalizeScopePath(relPath)
	if p == nil {
		return ScopeDecision{Allowed: true, Path: rel}
	}
	if g, ok := matchAny(p.deny, rel); ok {
		return ScopeDecision{Class: ScopeClassDeny, Rule: g.pattern, Path: rel}
	}
	if g, ok := matchAny(p.readOnly, rel); ok {
		return ScopeDecision{Class: ScopeClassReadOnly, Rule: g.pattern, Path: rel}
	}
	if len(p.allow) > 0 {
		if _, ok := matchAny(p.allow, rel); !ok {
			return ScopeDecision{Class: ScopeClassOutsideAllow, Path: rel}
		}
	}
	return ScopeDecision{Allowed: true, Path: rel}
}

// WithRoot returns a copy of the policy bound to the workspace rooted at root, so
// AllowsAnyWrite can consult the real tree. Callers that build a policy without a
// workspace (tests, harnesses) attach the root here; a nil policy returns nil.
//
// The root is made absolute — a relative one means the tree the caller is standing
// in — but is deliberately NOT symlink-resolved the way the jail resolves its own.
// EvalSymlinks on a root that does not exist walks up to the nearest real ancestor
// and hands back THAT directory, which would have AllowsAnyWrite walk a tree the
// caller never named and answer "yes, something is writable" for a workspace that
// has no files at all. A missing root stays the root it was given, and the walk
// reports the empty tree it describes. Only a root that cannot be made absolute at
// all becomes "", which AllowsAnyWrite reads as UNKNOWN and fails open on.
func (p *ScopePolicy) WithRoot(root string) *ScopePolicy {
	if p == nil {
		return nil
	}
	c := *p
	c.root = ""
	if abs, err := filepath.Abs(root); err == nil {
		c.root = filepath.Clean(abs)
	}
	return &c
}

// AllowsAnyWrite reports whether this policy leaves ANY path writable at all,
// for the tree rooted at root.
//
// It exists because a run launched with `--allow 'nothing/**'` — a pattern that
// matches no file in the tree — cannot change anything, and rails that tell the
// model to go and change something are then pushing it to do the one thing every
// write tool will refuse. Measured: such a validation run, prompted "You are the
// VALIDATOR. Do not edit any file.", degenerated into four repeated finish calls
// because the prompt QUOTES the spec it validates and so is full of change verbs.
// The task text can never be the signal for that; the write scope can, and it is
// already known.
//
// The answer is "at least one allow pattern matches a file that ACTUALLY EXISTS
// under root". It cannot be answered from the patterns alone: an earlier version
// probed a handful of synthetic paths ("x", "a/b/x", …) and reported that an
// ordinary scoped run — `--allow internal/tools/registry.go`, the common case —
// could write nothing, which would have switched the no-edit rails off for EVERY
// `--allow` run, exactly the ones that need them. A made-up path can never tell
// you whether a glob reaches the tree, so there is nothing to probe: walk it.
//
// The root is an explicit argument rather than the one attached by
// Workspace.WithScope, because an absent root has to fail open to true (see
// below) — which would make every false case unsatisfiable, and untestable, if
// the only way to name a tree were a policy built around a workspace.
//
// The walk asks CanWrite rather than re-reading the globs a second way, so deny
// and read_only keep their veto, and stops at the first writable path — the
// caller reads this ONCE per run (agent/loop.go), not per step, and a walk per
// step would be a real cost.
//
// An empty or unreadable root returns TRUE. Failing open keeps today's
// behaviour: a rail that stops firing is invisible, and silently disabling it
// because kloo could not stat the tree is worse than the false nudge it prevents.
func (p *ScopePolicy) AllowsAnyWrite(root string) bool {
	if p == nil {
		return true // no policy: the jail is the only boundary
	}
	if len(p.allow) == 0 {
		// "everything in the jail except what deny and read_only remove". A policy
		// with only deny/read_only globs still leaves the rest of the tree writable.
		return true
	}
	// "" is the UNKNOWN root — fail open, never silently disarm a rail. A bare
	// "." is not unknown: it is "the tree I am standing in", which is the only
	// tree a relative pattern can mean, and is what a test in this package gets.
	if root == "" {
		return true
	}
	// Absolutize the walk start, but do NOT resolve symlinks here. filepath.Rel is
	// purely lexical, so the answer must not depend on how the caller spelled the
	// root: Rel(".", "internal/x.go") is "internal/x.go" while
	// Rel(".", "./internal/x.go") is "../internal/x.go", and a pattern like
	// "internal/tools/registry.go" would match neither of two answers for the same
	// file. filepath.Join already cleans, so a relative root becomes one absolute
	// path and an absolute one is cleaned once; THAT string is then used as BOTH the
	// walk start and the Rel base, so the two can never disagree.
	//
	// EvalSymlinks is deliberately not called. It hands back the *target* directory
	// while WalkDir lstats the name it was handed, so for a workspace reached
	// through a symlink the base and the reported paths disagree, every rel comes
	// back with a "../" run that no allow glob can name, and the walk answers
	// "nothing is writable" for a tree full of writable files.
	start := root
	if !filepath.IsAbs(start) {
		wd, err := os.Getwd()
		if err != nil {
			return true
		}
		start = filepath.Join(wd, start) // Join cleans
	} else {
		start = filepath.Clean(start)
	}
	if st, err := os.Stat(start); err != nil || !st.IsDir() {
		return true // unreadable root: same reasoning
	}
	base := start
	found := false
	// The walk is bounded. This answers a rail decision ONCE per run, and the false
	// answer — the one the rail exists for — is exactly the case where the early
	// return never fires and the walk descends everything: a checkout with a
	// virtualenv or a build tree in it is hundreds of thousands of stats for a "no".
	// Enough of the tree to be confident is enough.
	const walkBudget = 50000
	var visited int
	// The root is the first entry WalkDir reports, and it lstats it: when the root
	// IS a symlink (a workspace linked into another tree, a checkout under a path
	// like /var -> /private/var) that entry is the LINK, reported as a
	// NON-DIRECTORY. It is only the doorway — no relative path to match, nothing to
	// judge — and judging it at all would either end the walk before it descended
	// anything (SkipDir on the root stops the whole walk) or answer "writable" from
	// a rel of "" that matches no allow glob.
	rootPath := base
	_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
		if visited++; visited > walkBudget {
			return filepath.SkipAll
		}
		// Clean before comparing or taking relative. WalkDir hands back the root
		// exactly as it was passed and the paths below it with the root's own
		// spelling, so one uncleaned segment in the root would put every rel outside
		// the base: filepath.Rel is purely lexical and answers
		// Rel("./internal/tools", "./internal/tools/registry.go") with
		// "../internal/tools/registry.go", which no allow glob can name.
		path = filepath.Clean(path)
		if path == rootPath {
			return nil
		}
		if err != nil {
			// A directory we cannot read must not answer "nothing is writable" —
			// that would disable the rails for the whole run on one bad subtree.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		// A symlink is never a doorway for this walk: WalkDir does not follow one,
		// so a link entry is a leaf. It is judged as the NAME it has in the tree,
		// not as the target it points at — which is exactly how the write tools see
		// it, since the jail resolves the path the model named.
		if d.IsDir() {
			// Skip the noise trees by NAME: generated and vendored content that no
			// allow glob points at, and the difference between a millisecond and a
			// second on a real checkout.
			if scopeWalkSkipDirs[d.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		// Match on the path RELATIVE TO THE ROOT, slash-normalized the way every
		// scope pattern is — exactly what CanWrite sees from the write tools. An
		// absolute rel matches no allow glob at all, and the walk would then report
		// "nothing is writable" for every scoped run: the same wrong answer the
		// probe paths gave, reached a different way. A Rel failure is not evidence
		// of "not writable" either, so that file is skipped, not counted as one.
		rel, relErr := filepath.Rel(base, path)
		if relErr != nil {
			return nil
		}
		if p.CanWrite(filepath.ToSlash(rel)).Allowed {
			found = true
			return filepath.SkipAll // one writable file is the whole answer
		}
		return nil
	})
	return found
}

// scopeError builds the *ScopeError for a denied decision on tool, with a bounded,
// model-facing message that names the path and the matching rule.
func scopeError(d ScopeDecision, tool string) *ScopeError {
	var msg string
	switch d.Class {
	case ScopeClassDeny:
		msg = fmt.Sprintf("write denied: %s is blocked by the scope deny rule %q; edit a file inside the allowed scope instead", d.Path, d.Rule)
	case ScopeClassReadOnly:
		msg = fmt.Sprintf("write denied: %s is marked read-only by the scope rule %q; it may be read but not modified", d.Path, d.Rule)
	case ScopeClassOutsideAllow:
		msg = fmt.Sprintf("write denied: %s is outside the allowed edit scope; only files matching --allow / .kloo/scope.yaml may be changed", d.Path)
	default:
		msg = fmt.Sprintf("write denied: %s is not permitted by the scope policy", d.Path)
	}
	return &ScopeError{Class: d.Class, Tool: tool, Path: d.Path, Rule: d.Rule, Message: msg}
}

// NormalizeScopePath canonicalizes a path for scope matching: backslashes to
// slashes, drop a leading "./" and any leading "/", and clean redundant separators.
// Patterns and checked paths both pass through this so matching compares like with
// like.
func NormalizeScopePath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	for strings.HasPrefix(p, "./") {
		p = p[2:]
	}
	p = strings.TrimPrefix(p, "/")
	// Collapse any "//" runs; a scope path never legitimately contains them.
	for strings.Contains(p, "//") {
		p = strings.ReplaceAll(p, "//", "/")
	}
	return p
}

// compileGlobs normalizes, de-duplicates blanks, and compiles a pattern list.
func compileGlobs(patterns []string) ([]scopeGlob, error) {
	out := make([]scopeGlob, 0, len(patterns))
	for _, raw := range patterns {
		pat := NormalizeScopePath(strings.TrimSpace(raw))
		if pat == "" {
			continue
		}
		re, err := compileGlob(pat)
		if err != nil {
			return nil, fmt.Errorf("tools: invalid scope glob %q: %w", raw, err)
		}
		out = append(out, scopeGlob{pattern: pat, re: re})
	}
	return out, nil
}

// matchAny returns the first glob in gs that matches rel.
func matchAny(gs []scopeGlob, rel string) (scopeGlob, bool) {
	for _, g := range gs {
		if g.re.MatchString(rel) {
			return g, true
		}
	}
	return scopeGlob{}, false
}

// compileGlob translates one slash-normalized glob into an anchored regexp with
// the documented semantics:
//
//   - "**"  — matches any number of path segments, including zero. A trailing
//     "/**" also matches the bare directory (so "src/**" matches "src" and
//     everything under it); a leading "**/" matches zero-or-more leading segments
//     (so "**/x.go" matches "x.go" and "a/b/x.go").
//   - "*"   — matches any run of characters WITHIN a single path segment (no "/").
//   - "?"   — matches a single non-"/" character.
//   - every other character is matched literally.
func compileGlob(pat string) (*regexp.Regexp, error) {
	// A trailing "/**" matches the bare directory AND everything under it, so
	// "src/**" matches "src", "src/a.go", and "src/x/y.go". Peel it off and append
	// an optional "/…" tail after translating the prefix.
	trailingDir := false
	if cut, ok := strings.CutSuffix(pat, "/**"); ok {
		pat = cut
		trailingDir = true
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); i++ {
		c := pat[i]
		switch c {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				i++ // consume the second '*'
				if i+1 < len(pat) && pat[i+1] == '/' {
					// "**/" — zero or more leading path segments.
					i++ // consume the '/'
					b.WriteString("(?:[^/]+/)*")
				} else {
					// "**" anywhere else — any run, across separators.
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*") // single-segment wildcard
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	if trailingDir {
		b.WriteString("(?:/.*)?")
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
