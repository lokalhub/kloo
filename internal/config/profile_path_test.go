package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// chroot points HOME, XDG_CONFIG_HOME and the CWD at a scratch tree so the search
// chain can be exercised without reading the developer's real profile. It returns
// the fake home and workspace.
func chroot(t *testing.T) (home, workspace string) {
	t.Helper()
	root := t.TempDir()
	home = filepath.Join(root, "home")
	workspace = filepath.Join(root, "repo")
	for _, d := range []string{home, workspace} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workspace); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return home, workspace
}

func writeProfileAt(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestProfileSearchPathsOrder pins the precedence the whole feature rests on:
// workspace-local beats the user's home, which beats the machine. Both accepted
// basenames appear in every directory, so a user who guesses either name in either
// place is still found.
func TestProfileSearchPathsOrder(t *testing.T) {
	home, workspace := chroot(t)
	got := ProfileSearchPaths(workspace)
	want := []string{
		filepath.Join(workspace, ".kloo", "kloo.json"),
		filepath.Join(workspace, ".kloo", "profiles.json"),
		filepath.Join(home, ".kloo", "kloo.json"),
		filepath.Join(home, ".kloo", "profiles.json"),
		filepath.Join(home, ".config", "kloo", "kloo.json"),
		filepath.Join(home, ".config", "kloo", "profiles.json"),
		filepath.Join(home, "etc", "kloo.json"),
		filepath.Join(home, "etc", "profiles.json"),
		"/etc/kloo.json",
		"/etc/profiles.json",
	}
	if !slices.Equal(got, want) {
		t.Errorf("search order mismatch\ngot:  %v\nwant: %v", got, want)
	}
}

// TestProfileSearchIncludesXDGWhenSet: XDG_CONFIG_HOME is consulted only when the
// user actually sets it, and it sits between ~/.kloo and the legacy ~/.config path.
func TestProfileSearchIncludesXDGWhenSet(t *testing.T) {
	_, workspace := chroot(t)
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	got := ProfileSearchPaths(workspace)
	i := slices.Index(got, "/xdg/kloo/kloo.json")
	if i < 0 {
		t.Fatalf("XDG path absent from %v", got)
	}
	if !strings.Contains(got[i-1], filepath.Join(".kloo", "profiles.json")) {
		t.Errorf("XDG should follow ~/.kloo, got %q before it", got[i-1])
	}
}

// TestFindProfilePrefersMostSpecific walks the chain from the bottom up: with only
// /etc-equivalents present the least specific wins, and each more specific file
// added takes over. This is the behaviour a user relies on to override a
// machine-wide profile per repo.
func TestFindProfilePrefersMostSpecific(t *testing.T) {
	home, workspace := chroot(t)
	// Ordered least → most specific. /etc is not writable in a test, so the lowest
	// rung exercised here is ~/etc.
	rungs := []string{
		filepath.Join(home, "etc", "kloo.json"),
		filepath.Join(home, ".config", "kloo", "kloo.json"),
		filepath.Join(home, ".kloo", "profiles.json"),
		filepath.Join(home, ".kloo", "kloo.json"),
		filepath.Join(workspace, ".kloo", "profiles.json"),
		filepath.Join(workspace, ".kloo", "kloo.json"),
	}
	for _, p := range rungs {
		writeProfileAt(t, p, `{}`)
		got, _ := FindProfile(workspace)
		if got != p {
			t.Errorf("after adding %s, FindProfile = %s, want %s", p, got, p)
		}
	}
}

// TestFindProfileSkipsDirectories: a DIRECTORY named kloo.json must not be returned
// as the profile — reading it would fail later with a confusing "is a directory"
// rather than falling through to the next real candidate.
func TestFindProfileSkipsDirectories(t *testing.T) {
	home, workspace := chroot(t)
	if err := os.MkdirAll(filepath.Join(workspace, ".kloo", "kloo.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(home, ".kloo", "kloo.json")
	writeProfileAt(t, real, `{}`)
	if got, _ := FindProfile(workspace); got != real {
		t.Errorf("FindProfile = %q, want the real file %q", got, real)
	}
}

// TestFindProfileNoneReturnsCandidates: on a total miss the path is empty but the
// candidate list still comes back, because that list IS the error message.
func TestFindProfileNoneReturnsCandidates(t *testing.T) {
	_, workspace := chroot(t)
	got, candidates := FindProfile(workspace)
	// /etc/kloo.json may genuinely exist on a real machine; only assert the
	// contract when it does not.
	if _, err := os.Stat("/etc/kloo.json"); err == nil {
		t.Skip("/etc/kloo.json exists on this machine")
	}
	if got != "" {
		t.Errorf("FindProfile = %q, want empty", got)
	}
	if len(candidates) == 0 {
		t.Error("candidate list is empty; the error message would name nowhere to look")
	}
}

// TestProviderWithoutAnyProfileErrors is the behaviour the feature exists for. A
// provider can only come from a profile, so with none found the run must fail
// naming the missing profile — not, as it did before, with `unknown --provider`,
// which sent the user hunting for a typo in a file that was never found.
func TestProviderWithoutAnyProfileErrors(t *testing.T) {
	_, workspace := chroot(t)
	if _, err := os.Stat("/etc/kloo.json"); err == nil {
		t.Skip("/etc/kloo.json exists on this machine")
	}
	_, err := Resolve(Flags{Provider: strp("lokalai")}, envFunc(nil), "")
	if err == nil {
		t.Fatal("Resolve succeeded with --provider and no profile anywhere")
	}
	if !errors.Is(err, ErrProfileNotFound) {
		t.Errorf("error does not wrap ErrProfileNotFound: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{
		"profile not found",
		"lokalai",
		filepath.Join(workspace, ".kloo", "kloo.json"),
		"/etc/kloo.json",
		"--profile",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message is missing %q:\n%s", want, msg)
		}
	}
}

// TestProviderFoundViaSearchChain: the provider resolves with NO --profile flag,
// purely from a discovered workspace-local profile. Without the search chain this
// same call failed.
func TestProviderFoundViaSearchChain(t *testing.T) {
	_, workspace := chroot(t)
	writeProfileAt(t, filepath.Join(workspace, ".kloo", "kloo.json"), `{
	  "providers": {
	    "lokalai": {
	      "endpoint": "https://example.invalid/v1",
	      "models": {"glm": "glm-5.3-flash"}
	    }
	  }
	}`)
	cfg, err := Resolve(Flags{Provider: strp("lokalai"), Model: strp("glm")}, envFunc(nil), "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if cfg.Endpoint != "https://example.invalid/v1" {
		t.Errorf("endpoint = %q, want the discovered profile's", cfg.Endpoint)
	}
	if cfg.Model != "glm-5.3-flash" {
		t.Errorf("model = %q, want the alias expanded", cfg.Model)
	}
}

// TestNoProfileStillRunsWithoutProvider guards the contract a search chain could
// easily break: kloo works with NO profile at all. Only --provider makes one
// mandatory, so a plain run must still resolve to the built-in defaults.
func TestNoProfileStillRunsWithoutProvider(t *testing.T) {
	chroot(t)
	cfg, err := Resolve(Flags{}, envFunc(nil), "")
	if err != nil {
		t.Fatalf("a run with no profile must not error: %v", err)
	}
	if cfg.Endpoint != DefaultEndpoint {
		t.Errorf("endpoint = %q, want the built-in default", cfg.Endpoint)
	}
}

// TestUnknownProviderNamesTheShadowingFile is the error that made this feature
// worth shipping. First-existing-wins means a stale profile high in the chain
// silently shadows the one the user maintains; "unknown --provider" then points at
// the wrong file and the user goes looking for a typo that is not there. The error
// must name the file it read AND the lower-precedence file that does define the
// provider. Reproduces the real case on the author's machine: a 56-byte June stub
// at ~/.config/kloo/profiles.json shadowing the maintained ~/etc/kloo.json.
func TestUnknownProviderNamesTheShadowingFile(t *testing.T) {
	home, _ := chroot(t)
	shadow := filepath.Join(home, ".config", "kloo", "profiles.json")
	real := filepath.Join(home, "etc", "kloo.json")
	writeProfileAt(t, shadow, `{ "snappy": { "temperature": 0.1 } }`)
	writeProfileAt(t, real, `{"providers":{"lokalai":{"endpoint":"https://example.invalid/v1"}}}`)

	_, err := Resolve(Flags{Provider: strp("lokalai")}, envFunc(nil), "")
	if err == nil {
		t.Fatal("Resolve succeeded against the shadowing profile")
	}
	msg := err.Error()
	for _, want := range []string{shadow, real, "shadows it", "--profile"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error message is missing %q:\n%s", want, msg)
		}
	}
	// It must NOT be reported as a missing profile: one was found, it just lacks
	// the provider. Conflating the two sends the user to create a second file.
	if errors.Is(err, ErrProfileNotFound) {
		t.Error("a found-but-incomplete profile was reported as ErrProfileNotFound")
	}
}

// TestUnknownProviderListsWhatIsDefined: with no shadowing candidate to suggest,
// the error still beats a bare "unknown provider" by listing the providers the
// profile does define — which is how a user spots a typo.
func TestUnknownProviderListsWhatIsDefined(t *testing.T) {
	path := writeProfile(t, `{"providers":{"openrouter":{"endpoint":"https://a.invalid"},"ollama":{"endpoint":"https://b.invalid"}}}`)
	_, err := Resolve(Flags{Provider: strp("lokalai")}, envFunc(nil), path)
	if err == nil {
		t.Fatal("Resolve succeeded with an undefined provider")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ollama, openrouter") {
		t.Errorf("error should list the defined providers in sorted order:\n%s", msg)
	}
}

// TestProfileSearchPathsDedupes: candidate directories genuinely collide — running
// kloo with the home directory as the workspace makes <workspace>/.kloo and ~/.kloo
// the same path, and XDG_CONFIG_HOME can point at ~/.config. The list must still
// name each path once, since it is printed verbatim in errors and `doctor`.
func TestProfileSearchPathsDedupes(t *testing.T) {
	home, _ := chroot(t)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	got := ProfileSearchPaths(home) // workspace == home: the colliding case
	seen := map[string]bool{}
	for _, p := range got {
		if seen[p] {
			t.Errorf("duplicate candidate %s in %v", p, got)
		}
		seen[p] = true
	}
	// Precedence must survive deduping: ~/.kloo still outranks ~/.config/kloo.
	if a, b := slices.Index(got, filepath.Join(home, ".kloo", "kloo.json")),
		slices.Index(got, filepath.Join(home, ".config", "kloo", "kloo.json")); a < 0 || b < 0 || a > b {
		t.Errorf("precedence lost after dedupe: ~/.kloo at %d, ~/.config/kloo at %d", a, b)
	}
}

// TestAliasWithoutProviderNamesTheFlag reproduces the reported incident exactly:
// `kloo --profile ~/etc/kloo.json --model glm` with no --provider. Before the
// guard the alias stayed literal, the endpoint fell back to 127.0.0.1:8080 and the
// key stayed unset, so kloo retried five times over 1m26s against a server that
// was not running — while the endpoint serving that model was healthy.
func TestAliasWithoutProviderNamesTheFlag(t *testing.T) {
	path := writeProfile(t, `{
	  "providers": {
	    "lokalai": {
	      "endpoint": "https://example.invalid/v1",
	      "models": {"glm": "glm-5.3-flash", "glimmer": "muse-glimmer-30b"}
	    }
	  }
	}`)
	_, err := Resolve(Flags{Model: strp("glm")}, envFunc(nil), path)
	if err == nil {
		t.Fatal("alias with no --provider resolved silently")
	}
	msg := err.Error()
	// With exactly one owner the message must give the literal flag to add, not a
	// list to choose from — that is the difference between a fix and a hint.
	if !strings.Contains(msg, "--provider lokalai") {
		t.Errorf("error should name the flag to add:\n%s", msg)
	}
}

// TestRealModelIDWithoutProviderIsSilent: the guard must fire ONLY on the mistake.
// A real model id that is nobody's alias runs against the default endpoint exactly
// as before — that is the ordinary llama.cpp/Ollama case and must not regress.
func TestRealModelIDWithoutProviderIsSilent(t *testing.T) {
	path := writeProfile(t, `{"providers":{"lokalai":{"models":{"glm":"glm-5.3-flash"}}}}`)
	cfg, err := Resolve(Flags{Model: strp("qwen2.5-coder-7b")}, envFunc(nil), path)
	if err != nil {
		t.Fatalf("a non-alias model must not trip the guard: %v", err)
	}
	if cfg.Model != "qwen2.5-coder-7b" || cfg.Endpoint != DefaultEndpoint {
		t.Errorf("model=%q endpoint=%q, want the id verbatim on the default endpoint",
			cfg.Model, cfg.Endpoint)
	}
}

// TestAliasWithExplicitEndpointIsAllowed: KLOO_ENDPOINT is the same "I mean this
// literally" signal as --endpoint, so the guard must respect it from the env too.
func TestAliasWithExplicitEndpointIsAllowed(t *testing.T) {
	path := writeProfile(t, `{"providers":{"lokalai":{"models":{"glm":"glm-5.3-flash"}}}}`)
	env := envFunc(map[string]string{EnvEndpoint: "http://example.invalid/v1"})
	cfg, err := Resolve(Flags{Model: strp("glm")}, env, path)
	if err != nil {
		t.Fatalf("KLOO_ENDPOINT must bypass the guard: %v", err)
	}
	if cfg.Model != "glm" {
		t.Errorf("model = %q, want the alias left literal", cfg.Model)
	}
}
