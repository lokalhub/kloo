package tui

import (
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/session"
)

// TestComposerPromptIsTheMark pins the prompt to the logo's ASCII form. If this
// ever drifts, the thing the user types at stops being the brand.
func TestComposerPromptIsTheMark(t *testing.T) {
	if composerPrompt != "k> " {
		t.Errorf("composer prompt = %q, want %q", composerPrompt, "k> ")
	}
	if !strings.HasPrefix(composerPrompt, brandASCII) {
		t.Errorf("composer prompt %q should start with the ASCII mark %q", composerPrompt, brandASCII)
	}
}

// TestBrandBannerUTF8 renders the box-drawing wordmark plus a tagline carrying
// the build version.
func TestBrandBannerUTF8(t *testing.T) {
	got := renderBrandBanner("v1.2.3", true)
	for _, want := range []string{"┬┌─", "├┴┐", "┴ ┴", brandTagline, "v1.2.3"} {
		if !strings.Contains(got, want) {
			t.Errorf("banner missing %q:\n%s", want, got)
		}
	}
	if lines := strings.Count(got, "\n"); lines != len(brandWordmark) {
		t.Errorf("banner has %d newlines, want %d (wordmark lines + tagline)", lines, len(brandWordmark))
	}
}

// TestBrandBannerASCIIFallback: without a UTF-8 locale the splash degrades to the
// plain mark rather than printing box-drawing the terminal can't show.
func TestBrandBannerASCIIFallback(t *testing.T) {
	got := renderBrandBanner("v1.2.3", false)
	if !strings.Contains(got, brandASCII) {
		t.Errorf("ASCII banner missing %q: %q", brandASCII, got)
	}
	for _, box := range []string{"┬", "├", "└", "─"} {
		if strings.Contains(got, box) {
			t.Errorf("ASCII banner leaked box-drawing %q: %q", box, got)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("ASCII banner should be a single line, got %q", got)
	}
}

// TestBrandBannerWithoutVersion: a bare build still renders a clean tagline (no
// dangling separator).
func TestBrandBannerWithoutVersion(t *testing.T) {
	got := renderBrandBanner("", true)
	if strings.Contains(got, "·") {
		t.Errorf("empty version should not render a separator: %q", got)
	}
}

// TestLocaleSupportsUTF8 pins POSIX precedence: the FIRST variable that is SET
// decides, so a non-UTF-8 LC_ALL beats a UTF-8 LANG rather than being ignored.
func TestLocaleSupportsUTF8(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"lang utf8", map[string]string{"LANG": "en_US.UTF-8"}, true},
		{"lang utf8 lowercase", map[string]string{"LANG": "en_us.utf8"}, true},
		{"lang C", map[string]string{"LANG": "C"}, false},
		{"lc_all wins over lang", map[string]string{"LC_ALL": "C", "LANG": "en_US.UTF-8"}, false},
		{"lc_ctype wins over lang", map[string]string{"LC_CTYPE": "C", "LANG": "en_US.UTF-8"}, false},
		{"lc_all utf8 with C lang", map[string]string{"LC_ALL": "en_US.UTF-8", "LANG": "C"}, true},
		{"empty lc_all falls through", map[string]string{"LC_ALL": "", "LANG": "en_US.UTF-8"}, true},
		{"nothing set", map[string]string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := localeSupportsUTF8(func(k string) string { return tc.env[k] })
			if got != tc.want {
				t.Errorf("localeSupportsUTF8(%v) = %v, want %v", tc.env, got, tc.want)
			}
		})
	}
}

// TestSplashOnFreshSessionOnly: a new session opens with the wordmark; a resumed
// one opens with its replayed conversation instead.
func TestSplashOnFreshSessionOnly(t *testing.T) {
	fresh := New(Config{Version: "v9.9.9", Getenv: func(string) string { return "en_US.UTF-8" }})
	if len(fresh.transcript) == 0 {
		t.Fatal("fresh session should open with a splash item")
	}
	if got := fresh.transcript[0].render(80); !strings.Contains(got, brandTagline) {
		t.Errorf("first item is not the splash: %q", got)
	}

	resumed := New(Config{
		Version: "v9.9.9",
		Getenv:  func(string) string { return "en_US.UTF-8" },
		History: []session.DisplayItem{{Kind: dispUser, Text: "earlier task"}},
	})
	if len(resumed.transcript) == 0 {
		t.Fatal("resumed session should replay its history")
	}
	if got := resumed.transcript[0].render(80); strings.Contains(got, brandTagline) {
		t.Errorf("resumed session should not open with the splash: %q", got)
	}
}
