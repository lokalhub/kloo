package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
)

// fakeLister serves a canned catalog (or an error) without an endpoint.
type fakeLister struct {
	models []llm.ModelInfo
	err    error
}

func (f fakeLister) Models(context.Context) ([]llm.ModelInfo, error) { return f.models, f.err }

func catalog() []llm.ModelInfo {
	return []llm.ModelInfo{
		{ID: "deepseek/deepseek-v4-flash", ContextLength: 1_048_576},
		{ID: "deepseek/deepseek-v3.2", ContextLength: 163_840},
		{ID: "qwen/qwen3-coder", ContextLength: 262_144},
	}
}

// collectLog returns a logf sink plus the slice it appends to.
func collectLog() (func(string, ...any), *[]string) {
	var lines []string
	return func(format string, args ...any) {
		lines = append(lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
	}, &lines
}

// TestAutoSizeRaisesWindowFromCatalog: the headline fix — a model whose window
// was never configured stops running at the 8000-token built-in default.
func TestAutoSizeRaisesWindowFromCatalog(t *testing.T) {
	cfg := config.Config{Model: "deepseek/deepseek-v4-flash", MaxContextTokens: 8000}
	logf, lines := collectLog()
	if err := applyModelInfo(context.Background(), &cfg, fakeLister{models: catalog()}, false, logf); err != nil {
		t.Fatalf("applyModelInfo: %v", err)
	}
	// 1048576 advertised − 20% reserve (209715, integer division) = 838861.
	if cfg.MaxContextTokens != 838_861 {
		t.Errorf("window = %d, want 838861 (advertised minus output headroom)", cfg.MaxContextTokens)
	}
	if len(*lines) == 0 || !strings.Contains((*lines)[0], "ctx-auto") {
		t.Errorf("resizing should be announced, got %v", *lines)
	}
}

// TestAutoSizeRespectsExplicitWindow: a window someone set on purpose — a capped
// local server, a profile entry — must never be silently overridden.
func TestAutoSizeRespectsExplicitWindow(t *testing.T) {
	cfg := config.Config{
		Model:                    "deepseek/deepseek-v4-flash",
		MaxContextTokens:         64_000,
		MaxContextTokensExplicit: true,
	}
	if err := applyModelInfo(context.Background(), &cfg, fakeLister{models: catalog()}, false, nil); err != nil {
		t.Fatalf("applyModelInfo: %v", err)
	}
	if cfg.MaxContextTokens != 64_000 {
		t.Errorf("window = %d, want the explicit 64000 left alone", cfg.MaxContextTokens)
	}
}

// TestAutoSizeNeverShrinks: a window already resolved above what the catalog
// implies stays put.
func TestAutoSizeNeverShrinks(t *testing.T) {
	cfg := config.Config{Model: "deepseek/deepseek-v3.2", MaxContextTokens: 900_000}
	if err := applyModelInfo(context.Background(), &cfg, fakeLister{models: catalog()}, false, nil); err != nil {
		t.Fatalf("applyModelInfo: %v", err)
	}
	if cfg.MaxContextTokens != 900_000 {
		t.Errorf("window = %d, want 900000 (auto-sizing must never shrink)", cfg.MaxContextTokens)
	}
}

// TestUnknownModelFailsFastWhenTheIDMatters is the dsv4 case. Warning and
// continuing was worse than useless: the request went out anyway and came back a
// 400 seconds later, with the provider's error instead of kloo's suggestions.
func TestUnknownModelFailsFastWhenTheIDMatters(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
	}{
		{"authenticated endpoint", config.Config{Model: "dsv4", APIKey: "sk-test", MaxContextTokens: 8000}},
		{"multi-model catalog", config.Config{Model: "dsv4", MaxContextTokens: 8000}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			err := applyModelInfo(context.Background(), &cfg, fakeLister{models: catalog()}, false, nil)
			if err == nil {
				t.Fatal("an unknown id should fail immediately, not after a round trip")
			}
			if !strings.Contains(err.Error(), "dsv4") {
				t.Errorf("error should name the model: %v", err)
			}
			if !strings.Contains(err.Error(), "deepseek/deepseek-v4-flash") {
				t.Errorf("error should suggest the near match: %v", err)
			}
		})
	}
}

// TestUnknownModelWarnsOnSingleModelEndpoint: a single-model llama.cpp server
// serves one catalog entry and ignores the model field, so kloo's out-of-the-box
// `--model local` must keep working.
func TestUnknownModelWarnsOnSingleModelEndpoint(t *testing.T) {
	single := []llm.ModelInfo{{ID: "qwen3-coder", ContextLength: 32768}}
	cfg := config.Config{Model: "local", MaxContextTokens: 8000}
	logf, lines := collectLog()
	if err := applyModelInfo(context.Background(), &cfg, fakeLister{models: single}, false, logf); err != nil {
		t.Fatalf("a single-model endpoint must not fail the run: %v", err)
	}
	if len(*lines) == 0 {
		t.Fatal("it should still warn — silence is the original bug")
	}
	if !strings.Contains((*lines)[0], "not in the endpoint's catalog") {
		t.Errorf("warning does not name the problem: %q", (*lines)[0])
	}
}

// TestStrictFailsEvenOnSingleModelEndpoint: strict is the override for the one
// case the default spares.
func TestStrictFailsEvenOnSingleModelEndpoint(t *testing.T) {
	single := []llm.ModelInfo{{ID: "qwen3-coder", ContextLength: 32768}}
	cfg := config.Config{Model: "local", MaxContextTokens: 8000}
	if err := applyModelInfo(context.Background(), &cfg, fakeLister{models: single}, true, nil); err == nil {
		t.Fatal("strict should fail even where the default warns")
	}
}

// TestCatalogUnavailableIsNeverFatal: most local servers don't implement /models.
// A missing catalog must change nothing, in either mode.
func TestCatalogUnavailableIsNeverFatal(t *testing.T) {
	for _, strict := range []bool{false, true} {
		cfg := config.Config{Model: "whatever-is-loaded", MaxContextTokens: 8000}
		err := applyModelInfo(context.Background(), &cfg,
			fakeLister{err: errors.New("404 page not found")}, strict, nil)
		if err != nil {
			t.Errorf("strict=%v: a missing catalog must not fail the run: %v", strict, err)
		}
		if cfg.MaxContextTokens != 8000 {
			t.Errorf("strict=%v: window changed without a catalog: %d", strict, cfg.MaxContextTokens)
		}
	}
}

// TestNearestModelIDs pins the suggestion quality that makes the warning useful.
func TestNearestModelIDs(t *testing.T) {
	cases := []struct {
		want  string
		first string
	}{
		{"deepseek", "deepseek/deepseek-v3.2"},        // family substring
		{"deepseek-v4", "deepseek/deepseek-v4-flash"}, // partial id
		{"qwen3-coder", "qwen/qwen3-coder"},           // tail substring
	}
	for _, tc := range cases {
		got := nearestModelIDs(tc.want, catalog())
		if len(got) == 0 {
			t.Errorf("%q produced no suggestions", tc.want)
			continue
		}
		if got[0] != tc.first {
			t.Errorf("%q → %v, want %q first", tc.want, got, tc.first)
		}
	}
	if got := nearestModelIDs("zzzzz", catalog()); len(got) != 0 {
		t.Errorf("an unrelated id should suggest nothing, got %v", got)
	}
}

// TestNearestModelIDsRescuesAnInitialism is the case that motivated the whole
// change: "dsv4" shares no substring and no prefix with any id, but it IS an
// in-order subsequence of "deepseek/deepseek-v4-flash". Without this tier the
// warning appears with no suggestion, which is barely more useful than silence.
func TestNearestModelIDsRescuesAnInitialism(t *testing.T) {
	got := nearestModelIDs("dsv4", catalog())
	if len(got) == 0 {
		t.Fatal("dsv4 produced no suggestions")
	}
	var found bool
	for _, g := range got {
		if g == "deepseek/deepseek-v4-flash" {
			found = true
		}
	}
	if !found {
		t.Errorf("suggestions %v should include deepseek/deepseek-v4-flash", got)
	}
}

// TestIsSubsequence pins the matcher: in-order, not contiguous, and never a
// match on the empty string (which would suggest every model in the catalog).
func TestIsSubsequence(t *testing.T) {
	cases := []struct {
		want, s string
		ok      bool
	}{
		{"dsv4", "deepseek/deepseek-v4-flash", true},
		{"qc", "qwen/qwen3-coder", true},
		{"abc", "aXbXc", true},
		{"cba", "aXbXc", false}, // order matters
		{"abcd", "abc", false},
		{"", "anything", false},
	}
	for _, tc := range cases {
		if got := isSubsequence(tc.want, tc.s); got != tc.ok {
			t.Errorf("isSubsequence(%q, %q) = %v, want %v", tc.want, tc.s, got, tc.ok)
		}
	}
}

// TestAutoSizedWindowReserve pins the output headroom rule: 20%, floored at 8192
// so a small window still leaves room to generate.
func TestAutoSizedWindowReserve(t *testing.T) {
	cases := []struct{ advertised, want int }{
		{1_048_576, 838_861}, // 20% reserve dominates
		{163_840, 131_072},
		{32_768, 24_576}, // 8192 floor dominates
		{16_384, 8_192},
	}
	for _, tc := range cases {
		if got := autoSizedWindow(tc.advertised); got != tc.want {
			t.Errorf("autoSizedWindow(%d) = %d, want %d", tc.advertised, got, tc.want)
		}
	}
}

// TestUnknownModelSuggestsAnAlias: when a provider is selected, the error must
// name the remedy, not just the problem. A short name that resolves to nothing is
// almost always a missing alias, and the fix is one line of profile — printing it
// is the difference between an error you can act on and one you have to research.
func TestUnknownModelSuggestsAnAlias(t *testing.T) {
	cfg := config.Config{
		Model:            "dsv4",
		Provider:         "openrouter",
		APIKey:           "sk-test",
		MaxContextTokens: 8000,
	}
	err := applyModelInfo(context.Background(), &cfg, fakeLister{models: catalog()}, false, nil)
	if err == nil {
		t.Fatal("an unknown id on an authenticated endpoint should fail")
	}
	for _, want := range []string{
		`"models"`,                   // names the mechanism
		`"openrouter"`,               // scoped to the selected provider
		`"dsv4"`,                     // the name they typed
		"deepseek/deepseek-v4-flash", // what to point it at
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %s:\n%v", want, err)
		}
	}
}

// TestNoAliasHintWithoutProvider: aliases are provider-scoped, so with no
// --provider there is nowhere to put one and the hint would be misleading.
func TestNoAliasHintWithoutProvider(t *testing.T) {
	cfg := config.Config{Model: "dsv4", APIKey: "sk-test", MaxContextTokens: 8000}
	err := applyModelInfo(context.Background(), &cfg, fakeLister{models: catalog()}, false, nil)
	if err == nil {
		t.Fatal("expected the unknown-model failure")
	}
	if strings.Contains(err.Error(), `"models"`) {
		t.Errorf("no provider selected — the alias hint has nowhere to go:\n%v", err)
	}
}

// TestNoAliasHintWithoutSuggestions: with no near match there is no sensible
// target, and an alias pointing nowhere is worse than no advice.
func TestNoAliasHintWithoutSuggestions(t *testing.T) {
	cfg := config.Config{Model: "zzzzz", Provider: "openrouter", APIKey: "sk-test", MaxContextTokens: 8000}
	err := applyModelInfo(context.Background(), &cfg, fakeLister{models: catalog()}, false, nil)
	if err == nil {
		t.Fatal("expected the unknown-model failure")
	}
	if strings.Contains(err.Error(), `"models"`) {
		t.Errorf("no suggestion to alias to — the hint should be suppressed:\n%v", err)
	}
}
