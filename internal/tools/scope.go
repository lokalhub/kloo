package tools

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

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
type ScopePolicy struct {
	allow    []scopeGlob
	deny     []scopeGlob
	readOnly []scopeGlob
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
