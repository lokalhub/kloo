// modelinfo.go resolves what the endpoint actually knows about the selected
// model, from a single /models fetch shared by the TUI and headless paths.
//
// It answers two questions that used to fail silently and separately:
//
//   - Is this model id real? A typo or a short name the endpoint has never heard
//     of ("dsv4" instead of "deepseek/deepseek-v4-flash") used to match no profile
//     entry and no bundled row, and fall through to the 8000-token built-in
//     default — so a million-token model ran as an 8k one and nothing said a word.
//   - How big is its window? The catalog carries context_length, so the answer is
//     already in the response we fetch for the first question.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
)

// modelInfo is what the catalog told us about the selected model.
type modelInfo struct {
	// CatalogOK is false when the endpoint served no usable catalog. Many local
	// servers don't implement /models, so this is normal and never fatal.
	CatalogOK bool
	// Known reports whether the selected id appeared in the catalog.
	Known bool
	// Window is the advertised context_length (0 when unknown).
	Window int
	// Suggestions are catalog ids that look like what the user meant.
	Suggestions []string
	// CatalogSize is how many models the endpoint listed. A server listing exactly
	// one is the single-model llama.cpp case, which ignores the model field
	// entirely; more than one means the id is a real selector and a wrong one is a
	// genuine error.
	CatalogSize int
}

// autoSizeMinReserve is the floor on output headroom held back from an advertised
// window: generation + tool results + verify traces still have to fit.
const autoSizeMinReserve = 8192

// lookupModelInfo fetches the catalog once and reports what it says about
// cfg.Model. Any fetch failure yields CatalogOK=false, never an error: a run must
// not depend on an optional endpoint feature.
func lookupModelInfo(ctx context.Context, cfg config.Config, lister modelLister) modelInfo {
	models, err := lister.Models(ctx)
	if err != nil || len(models) == 0 {
		return modelInfo{}
	}
	info := modelInfo{CatalogOK: true, CatalogSize: len(models)}
	for _, m := range models {
		if m.ID == cfg.Model {
			info.Known, info.Window = true, m.ContextLength
			return info
		}
	}
	info.Suggestions = nearestModelIDs(cfg.Model, models)
	return info
}

// nearestModelIDs picks catalog ids that plausibly match what the user typed:
// substring matches in either direction, then a shared-prefix fallback so a
// mistyped tail still surfaces the family. Deterministic (sorted) and capped.
func nearestModelIDs(want string, models []llm.ModelInfo) []string {
	w := strings.ToLower(want)
	var hits []string
	for _, m := range models {
		id := strings.ToLower(m.ID)
		if strings.Contains(id, w) || strings.Contains(w, id) {
			hits = append(hits, m.ID)
		}
	}
	if len(hits) == 0 {
		// Shared leading run of at least 3 characters — catches "deepseek-v4"
		// against "deepseek/deepseek-v4-flash".
		for _, m := range models {
			if n := commonPrefixLen(strings.ToLower(m.ID), w); n >= 3 {
				hits = append(hits, m.ID)
			}
		}
	}
	if len(hits) == 0 {
		// Last tier: in-order subsequence, the fuzzy-finder rule. This is what
		// rescues an initialism — "dsv4" IS a subsequence of
		// "deepseek/deepseek-v4-flash", and that abbreviation is precisely the kind
		// of thing someone types and then wonders why the window came out at 8000.
		for _, m := range models {
			if isSubsequence(w, strings.ToLower(m.ID)) {
				hits = append(hits, m.ID)
			}
		}
		// Subsequence matching is loose, so rank tightest-first: a shorter id
		// holding the same letters is the closer guess.
		sort.Slice(hits, func(i, j int) bool {
			if len(hits[i]) != len(hits[j]) {
				return len(hits[i]) < len(hits[j])
			}
			return hits[i] < hits[j]
		})
	} else {
		sort.Strings(hits)
	}
	if len(hits) > 5 {
		hits = hits[:5]
	}
	return hits
}

// isSubsequence reports whether every byte of want appears in s, in order.
func isSubsequence(want, s string) bool {
	if want == "" {
		return false
	}
	i := 0
	for j := 0; j < len(s); j++ {
		if s[j] == want[i] {
			if i++; i == len(want) {
				return true
			}
		}
	}
	return false
}

func commonPrefixLen(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// applyModelInfo validates the selected model against the endpoint catalog and,
// when the window was not set deliberately, sizes it from the model's advertised
// context_length.
//
// An unknown model FAILS THE RUN whenever the id demonstrably matters. Warning
// and continuing was worse than useless: the request went out anyway and came
// back a 400 seconds later, with a provider error instead of kloo's suggestions.
//
// The id matters when the endpoint authenticates (every hosted provider rejects
// an unknown id) or when it lists more than one model (a real selector — a typo
// against llama-swap picks nothing). It does NOT matter for a single-model
// llama.cpp server, which serves one entry and ignores the model field; that is
// kloo's out-of-the-box path and must keep working, so it warns instead. Pass
// strict to fail there too.
func applyModelInfo(ctx context.Context, cfg *config.Config, lister modelLister, strict bool, logf func(string, ...any)) error {
	info := lookupModelInfo(ctx, *cfg, lister)
	if !info.CatalogOK {
		return nil // endpoint serves no catalog — nothing to validate or size against
	}

	if !info.Known {
		msg := fmt.Sprintf("model %q is not in the endpoint's catalog", cfg.Model)
		if len(info.Suggestions) > 0 {
			msg += fmt.Sprintf(" — did you mean %s?", strings.Join(info.Suggestions, ", "))
		}
		msg += aliasHint(cfg, info)
		if strict || modelIDMatters(cfg, info) {
			return errors.New(msg)
		}
		if logf != nil {
			logf("kloo: %s (continuing; this endpoint appears to ignore the model id)", msg)
		}
		return nil
	}

	if info.Window <= 0 || cfg.MaxContextTokensExplicit {
		return nil
	}
	sized := autoSizedWindow(info.Window)
	if sized <= cfg.MaxContextTokens {
		return nil // never shrink a window someone already resolved higher
	}
	if logf != nil {
		logf("kloo: ctx-auto · %s advertises %d ctx → window %d (reserved %d for output)",
			cfg.Model, info.Window, sized, info.Window-sized)
	}
	cfg.MaxContextTokens = sized
	return nil
}

// aliasHint suggests the profile edit that turns a short name the user clearly
// MEANT as an alias into a working one.
//
// This is the gap that made the original report hard to see: a provider with no
// "models" block has no alias to expand, so a short id falls through as a literal
// model name, and the error only said the id was unknown — never that a mechanism
// for short names exists, or where to configure it. The remedy is one line of
// profile, so the error may as well print it.
//
// Aliases are provider-scoped, so this stays silent when no provider is selected,
// and it needs a suggestion to name a target worth aliasing to.
func aliasHint(cfg *config.Config, info modelInfo) string {
	if cfg.Provider == "" || len(info.Suggestions) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"\n\nIf %q was meant as a short name, alias it in your profile (point it at whichever of the above you want):\n"+
			"  \"providers\": { %q: { \"models\": { %q: %q } } }",
		cfg.Model, cfg.Provider, cfg.Model, info.Suggestions[0])
}

// modelIDMatters reports whether a wrong id will actually break the run, so an
// unknown one can fail immediately instead of after a round trip.
func modelIDMatters(cfg *config.Config, info modelInfo) bool {
	return cfg.APIKey != "" || info.CatalogSize > 1
}

// autoSizedWindow reserves output headroom from an advertised window and applies
// the optional KLOO_CTX_AUTO_CAP ceiling (for a server whose real limit is below
// what the catalog claims — e.g. a Vulkan-limited local build).
func autoSizedWindow(advertised int) int {
	reserve := advertised / 5
	if reserve < autoSizeMinReserve {
		reserve = autoSizeMinReserve
	}
	sized := advertised - reserve
	if capStr := os.Getenv("KLOO_CTX_AUTO_CAP"); capStr != "" {
		if capN, err := strconv.Atoi(capStr); err == nil && capN > 0 && sized > capN {
			sized = capN
		}
	}
	if sized < 0 {
		sized = 0
	}
	return sized
}
