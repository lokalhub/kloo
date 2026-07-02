package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ScopeConfig is the resolved model-facing file-scope policy (A1/A2): the glob
// lists the CLI turns into a tools.ScopePolicy. Empty lists ⇒ no scope (the
// workspace jail is the only boundary).
type ScopeConfig struct {
	Allow    []string
	Deny     []string
	ReadOnly []string
}

// Active reports whether any glob constrains the scope.
func (s ScopeConfig) Active() bool {
	return len(s.Allow) > 0 || len(s.Deny) > 0 || len(s.ReadOnly) > 0
}

// ScopeManifestFile is the optional per-workspace scope manifest, resolved
// relative to the workspace root.
const ScopeManifestFile = ".kloo/scope.yaml"

// ScopeFlags are the raw CLI scope overrides (--allow/--deny/--read-only). A nil
// slice means "flag not set" (so the manifest's value for that key stands); a
// non-nil slice (even empty) REPLACES the manifest's list for that key — the
// override is a replacement, never a silent append (A1).
type ScopeFlags struct {
	Allow    []string
	Deny     []string
	ReadOnly []string
}

// ResolveScope loads the optional .kloo/scope.yaml under workspaceDir and overlays
// the CLI flags key-by-key: a non-nil flag list replaces that key's manifest list;
// a nil flag list leaves the manifest value in place. A missing manifest is not an
// error (flags alone still apply). A malformed manifest returns an error wrapping
// ErrProfileParse-style context. All patterns are trimmed and slash-normalized.
func ResolveScope(flags ScopeFlags, workspaceDir string) (ScopeConfig, error) {
	manifest, err := loadScopeManifest(filepath.Join(workspaceDir, ScopeManifestFile))
	if err != nil {
		return ScopeConfig{}, err
	}
	out := ScopeConfig{
		Allow:    manifest.Allow,
		Deny:     manifest.Deny,
		ReadOnly: manifest.ReadOnly,
	}
	if flags.Allow != nil {
		out.Allow = flags.Allow
	}
	if flags.Deny != nil {
		out.Deny = flags.Deny
	}
	if flags.ReadOnly != nil {
		out.ReadOnly = flags.ReadOnly
	}
	out.Allow = cleanGlobList(out.Allow)
	out.Deny = cleanGlobList(out.Deny)
	out.ReadOnly = cleanGlobList(out.ReadOnly)
	return out, nil
}

// cleanGlobList trims whitespace and drops blank entries, preserving order.
func cleanGlobList(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, p := range in {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadScopeManifest parses the narrow .kloo/scope.yaml shape. There is no YAML
// dependency in kloo (no-CGO, stdlib-first — see the task decisions.md), and this
// file has exactly three known keys each holding a list of string globs, so a tiny
// purpose-built parser is used instead of adding a library. Supported forms:
//
//	allow:
//	  - "src/**"
//	  - lib/**
//	deny: [".env", "dist/**"]
//	read_only:
//	  - tests/**
//
// A missing file yields the zero ScopeConfig and no error. Any unknown top-level
// key is ignored (forward-compatible); a value that is neither a block list nor an
// inline [ ... ] list under a known key is an error.
func loadScopeManifest(path string) (ScopeConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return ScopeConfig{}, nil
		}
		return ScopeConfig{}, fmt.Errorf("config: read scope manifest %s: %w", path, err)
	}
	var cfg ScopeConfig
	lines := strings.Split(string(data), "\n")
	curKey := "" // which known key we are collecting block-list items for
	target := func(key string) *[]string {
		switch key {
		case "allow":
			return &cfg.Allow
		case "deny":
			return &cfg.Deny
		case "read_only":
			return &cfg.ReadOnly
		default:
			return nil
		}
	}
	for n, raw := range lines {
		line := stripYAMLComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.HasPrefix(strings.TrimSpace(line), "- ") || strings.TrimSpace(line) == "-" {
			// A block-list item belongs to the current key.
			if curKey == "" {
				continue // stray item under an unknown/ignored key
			}
			if dst := target(curKey); dst != nil {
				item := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "-"))
				if v := unquoteYAML(item); v != "" {
					*dst = append(*dst, v)
				}
			}
			continue
		}
		// A "key: value" line. Only a top-level (unindented) key starts a new section.
		key, rest, ok := strings.Cut(line, ":")
		if !ok {
			return ScopeConfig{}, fmt.Errorf("config: scope manifest %s line %d: expected 'key: value' or '- item', got %q", path, n+1, strings.TrimSpace(raw))
		}
		key = strings.TrimSpace(key)
		curKey = key
		rest = strings.TrimSpace(rest)
		dst := target(key)
		if dst == nil {
			continue // unknown key: ignore, and its indented items are skipped above
		}
		if rest == "" {
			continue // block-list form; items follow on subsequent lines
		}
		items, err := parseInlineList(rest)
		if err != nil {
			return ScopeConfig{}, fmt.Errorf("config: scope manifest %s line %d (%s): %w", path, n+1, key, err)
		}
		*dst = append(*dst, items...)
	}
	return cfg, nil
}

// stripYAMLComment removes a trailing " # comment" that is not inside quotes.
func stripYAMLComment(line string) string {
	inS, inD := false, false
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\'':
			if !inD {
				inS = !inS
			}
		case '"':
			if !inS {
				inD = !inD
			}
		case '#':
			if !inS && !inD && (i == 0 || line[i-1] == ' ' || line[i-1] == '\t') {
				return line[:i]
			}
		}
	}
	return line
}

// parseInlineList parses an inline "[a, b, c]" list into its (unquoted) items.
func parseInlineList(s string) ([]string, error) {
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("expected an inline list [a, b] or a block list of '- item' lines, got %q", s)
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return nil, nil
	}
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := unquoteYAML(strings.TrimSpace(p)); v != "" {
			out = append(out, v)
		}
	}
	return out, nil
}

// unquoteYAML strips a matching pair of single or double quotes.
func unquoteYAML(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// StopPolicy is the resolved A7 hard-stop configuration (--stop-on). Every trigger
// is DETECTABLE (no subjective heuristics): an off-scope edit, a read-only write,
// or N repeated verifier failures.
type StopPolicy struct {
	OffScopeEdit   bool // stop on the first A1 off-scope denial (incl. a scoped run_command rejection)
	ReadOnlyEdit   bool // stop on the first A2 read-only write attempt
	RepeatedVerify int  // stop after this many repeated verifier failures (0 ⇒ off)
}

// Active reports whether any stop rule is configured.
func (s StopPolicy) Active() bool {
	return s.OffScopeEdit || s.ReadOnlyEdit || s.RepeatedVerify > 0
}

// parseStopOn parses the --stop-on rule tokens (already comma-split by cobra's
// StringSlice) into a StopPolicy. Valid rules:
//
//	off-scope-edit
//	read-only-edit
//	repeated-verify=N   (N a positive integer)
//
// An unknown rule name or a malformed/absent threshold is a config error (the CLI
// surfaces it as failure_code config_error). Duplicate rules are idempotent.
func parseStopOn(rules []string) (StopPolicy, error) {
	var sp StopPolicy
	for _, raw := range rules {
		rule := strings.TrimSpace(raw)
		if rule == "" {
			continue
		}
		name, val, hasVal := cutRuleValue(rule)
		switch name {
		case "off-scope-edit":
			if hasVal {
				return StopPolicy{}, fmt.Errorf("config: invalid --stop-on rule %q (off-scope-edit takes no value)", raw)
			}
			sp.OffScopeEdit = true
		case "read-only-edit":
			if hasVal {
				return StopPolicy{}, fmt.Errorf("config: invalid --stop-on rule %q (read-only-edit takes no value)", raw)
			}
			sp.ReadOnlyEdit = true
		case "repeated-verify":
			n, err := strconv.Atoi(strings.TrimSpace(val))
			if !hasVal || err != nil || n <= 0 {
				return StopPolicy{}, fmt.Errorf("config: invalid --stop-on rule %q (want repeated-verify=N with N a positive integer)", raw)
			}
			sp.RepeatedVerify = n
		default:
			return StopPolicy{}, fmt.Errorf("config: unknown --stop-on rule %q (valid: off-scope-edit, read-only-edit, repeated-verify=N)", raw)
		}
	}
	return sp, nil
}

// cutRuleValue splits a stop rule on the first '=' or ':' separator.
func cutRuleValue(rule string) (name, val string, hasVal bool) {
	if i := strings.IndexAny(rule, "=:"); i >= 0 {
		return strings.TrimSpace(rule[:i]), rule[i+1:], true
	}
	return rule, "", false
}
