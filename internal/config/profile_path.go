package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrProfileNotFound is returned when a profile is REQUIRED for the requested run
// but no file was found at any searched location. It is deliberately not returned
// for an ordinary run: kloo works with no profile at all (built-in defaults plus
// --endpoint/--model), so "absent" is only fatal when something in the run can
// only come from a profile — today that is --provider, whose endpoint, key and
// model aliases have no other source.
var ErrProfileNotFound = errors.New("profile not found")

// profileFileNames are the two accepted basenames, newest first. "kloo.json" is
// the name kloo has used for a hand-written profile since the provider block was
// added; "profiles.json" is the original and stays supported so existing installs
// keep working. Both are checked in every directory below, so a user who guesses
// either name in either place gets a working setup.
var profileFileNames = []string{"kloo.json", "profiles.json"}

// profileSearchDirs returns the directories consulted for a profile, MOST SPECIFIC
// FIRST: a workspace-local profile beats the user's, which beats the machine's.
// That ordering is the point — it lets one repo pin a provider (a different
// endpoint, a different model alias) without touching the user's own config, the
// same way .git, .editorconfig and node_modules resolve.
//
// workspace is the directory kloo is operating on; pass "" to skip the
// workspace-local entries (e.g. when no workspace is known yet).
func profileSearchDirs(workspace string) []string {
	var dirs []string
	if workspace != "" {
		dirs = append(dirs, filepath.Join(workspace, ".kloo"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".kloo"))
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			dirs = append(dirs, filepath.Join(xdg, "kloo"))
		}
		dirs = append(dirs,
			filepath.Join(home, ".config", "kloo"), // legacy XDG-style location
			filepath.Join(home, "etc"),             // ~/etc/kloo.json
		)
	}
	return append(dirs, "/etc") // machine-wide, last
}

// ProfileSearchPaths returns every path kloo consults when --profile is not given,
// in precedence order. It stats nothing — it is the candidate list, suitable for
// an error message or `kloo doctor`.
func ProfileSearchPaths(workspace string) []string {
	dirs := profileSearchDirs(workspace)
	paths := make([]string, 0, len(dirs)*len(profileFileNames))
	// Dedupe, keeping the FIRST (highest-precedence) occurrence. Two candidate dirs
	// can collide for real: running kloo with the home directory as the workspace
	// makes <workspace>/.kloo and ~/.kloo the same path, and XDG_CONFIG_HOME can be
	// set to ~/.config. Without this the search still works but the error message
	// and `doctor` list the same path twice, which reads like a bug.
	seen := make(map[string]bool, cap(paths))
	for _, d := range dirs {
		for _, name := range profileFileNames {
			p := filepath.Join(d, name)
			if seen[p] {
				continue
			}
			seen[p] = true
			paths = append(paths, p)
		}
	}
	return paths
}

// FindProfile returns the first EXISTING profile path from ProfileSearchPaths,
// along with the full candidate list that was searched. The path is "" when none
// exists; the candidate list is returned either way so a caller can report what it
// looked for. A directory sitting at a candidate path is skipped rather than
// returned, since reading it would fail later with a confusing error.
func FindProfile(workspace string) (string, []string) {
	candidates := ProfileSearchPaths(workspace)
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, candidates
		}
	}
	return "", candidates
}

// profileNotFoundError builds the user-facing error: it names why a profile was
// needed and lists every path searched, in order, so the fix is "create one of
// these" with no guessing. The returned error satisfies
// errors.Is(err, ErrProfileNotFound).
func profileNotFoundError(reason string, candidates []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, " — %s, and no profile exists at any of:", reason)
	for _, p := range candidates {
		fmt.Fprintf(&b, "\n  %s", p)
	}
	b.WriteString("\n\nCreate one of those, or pass --profile <path>.")
	return fmt.Errorf("config: %w%s", ErrProfileNotFound, b.String())
}

// defaultProfilePath resolves the profile path used when --profile is unset: the
// first existing candidate, else the preferred location (~/.kloo/kloo.json) so
// callers have a concrete path to report. A missing profile is NOT an error here —
// Resolve treats an absent profile as "use defaults" (see ErrProfileNotFound).
func defaultProfilePath() (string, error) {
	workspace, _ := os.Getwd()
	if found, _ := FindProfile(workspace); found != "" {
		return found, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kloo", profileFileNames[0]), nil
}

// DefaultProfilePathForDiagnostics returns the profile path Resolve would inspect
// when --profile is unset. It performs no profile parsing and does not require the
// file to exist.
func DefaultProfilePathForDiagnostics() (string, error) {
	return defaultProfilePath()
}

// workspaceDir is the directory the workspace-local profile lookup is relative to.
// Resolve is not given a workspace, and the process CWD is the workspace for every
// way kloo is launched today (the CLI runs in the repo it edits), so CWD is the
// honest answer rather than a guess. An unreadable CWD yields "", which simply
// skips the workspace-local candidates.
func workspaceDir() string {
	wd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return wd
}

// unknownProviderError explains a provider that the profile kloo actually read
// does not define. Naming the provider alone is a dead end whenever more than one
// candidate profile exists: the search stops at the FIRST existing file, so a stale
// leftover high in the chain silently shadows the profile the user maintains, and
// "unknown --provider" then points at the wrong file. So this also says which file
// was read, and — the part that turns a dead end into a fix — names any OTHER
// candidate that does define the provider.
func unknownProviderError(provider, profilePath string, found map[string]providerEntry) error {
	var b strings.Builder
	fmt.Fprintf(&b, "config: unknown --provider %q", provider)

	read := profilePath
	searchedChain := profilePath == ""
	if searchedChain {
		read, _ = FindProfile(workspaceDir())
	}
	if read != "" {
		fmt.Fprintf(&b, " in %s", read)
		if searchedChain {
			b.WriteString(" (found by search)")
		}
	}
	if names := sortedKeys(found); len(names) > 0 {
		fmt.Fprintf(&b, "\n  that profile defines: %s", strings.Join(names, ", "))
	} else {
		b.WriteString("\n  that profile defines no providers at all")
	}

	// The actionable part: a lower-precedence profile that HAS the provider.
	if searchedChain && read != "" {
		for _, cand := range ProfileSearchPaths(workspaceDir()) {
			if cand == read {
				continue
			}
			other, err := loadProviders(cand)
			if err != nil {
				continue // malformed or absent: not a suggestion worth making
			}
			if _, ok := other[provider]; ok {
				fmt.Fprintf(&b, "\n\n  %s DOES define %q, but %s is searched first and shadows it.",
					cand, provider, read)
				fmt.Fprintf(&b, "\n  Use --profile %s, or remove the shadowing file.", cand)
				break
			}
		}
	}
	b.WriteString("\n\nDefine it under \"providers\" in the profile, or pass --profile <path>.")
	return errors.New(b.String())
}

// sortedKeys returns a provider map's names in a stable order, so the error message
// is deterministic (map iteration is not).
func sortedKeys(m map[string]providerEntry) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// providersDefiningAlias returns the names of the providers in the profile whose
// "models" block defines alias, sorted. Empty when the profile has none — which
// includes the ordinary case of a real model id that is nobody's alias, so this
// stays silent for every run that is not the mistake it is looking for.
func providersDefiningAlias(profilePath, alias string) []string {
	if alias == "" {
		return nil
	}
	providers, err := loadProviders(profilePath)
	if err != nil {
		return nil // malformed profile is reported elsewhere; do not double-fault
	}
	var owners []string
	for name, p := range providers {
		if real, ok := p.Models[alias]; ok && real != "" {
			owners = append(owners, name)
		}
	}
	sort.Strings(owners)
	return owners
}

// aliasNeedsProviderError explains a model alias used without --provider. Aliases
// are provider-scoped by design (the same short name can mean different things on
// different providers), so kloo cannot pick one for the user — but it can say
// exactly which flag to add, which is the whole difference between this and five
// retries against localhost.
func aliasNeedsProviderError(alias string, owners []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "config: --model %q is a model alias, but no --provider was given", alias)
	b.WriteString("\n  Aliases are defined per provider, so without one kloo would send")
	fmt.Fprintf(&b, " %q verbatim\n  to the default endpoint (%s) instead of the provider's.\n", alias, DefaultEndpoint)
	switch len(owners) {
	case 1:
		fmt.Fprintf(&b, "\n  Add: --provider %s", owners[0])
		fmt.Fprintf(&b, "\n  Or set it once in the profile: \"defaultProvider\": %q", owners[0])
	default:
		fmt.Fprintf(&b, "\n  %q is defined by: %s\n  Add --provider with the one you want,",
			alias, strings.Join(owners, ", "))
		b.WriteString("\n  or set \"defaultProvider\" in the profile to pick one for every run.")
	}
	fmt.Fprintf(&b, "\n\n  Or pass --endpoint explicitly to use %q as a literal model id.", alias)
	return errors.New(b.String())
}

// unknownDefaultProviderError explains a "defaultProvider" that names a provider
// the profile does not define. Reporting this as a bad --provider would be
// actively misleading: the user passed no flag, so they would go looking at their
// command line instead of at the one line in the file that is wrong.
func unknownDefaultProviderError(provider, profilePath string, found map[string]providerEntry) error {
	path := profilePath
	if path == "" {
		path, _ = FindProfile(workspaceDir())
	}
	var b strings.Builder
	fmt.Fprintf(&b, "config: %q sets \"defaultProvider\": %q, but defines no such provider", path, provider)
	if names := sortedKeys(found); len(names) > 0 {
		fmt.Fprintf(&b, "\n  it defines: %s", strings.Join(names, ", "))
	} else {
		b.WriteString("\n  it defines no providers at all")
	}
	b.WriteString("\n\nFix \"defaultProvider\", or override it for this run with --provider.")
	return errors.New(b.String())
}
