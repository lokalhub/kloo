package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestPaletteConstructs: every semantic style renders a probe string without
// panicking and the probe text survives (colour is stripped under the ascii test
// profile, so we assert the content, not escapes).
func TestPaletteConstructs(t *testing.T) {
	styles := map[string]lipgloss.Style{
		"accent":  accent,
		"success": success,
		"danger":  danger,
		"warning": warning,
		"muted":   muted,
	}
	for name, st := range styles {
		const probe = "probe"
		got := st.Render(probe)
		if !strings.Contains(got, probe) {
			t.Errorf("style %q dropped its content: %q", name, got)
		}
	}
}

// TestAccentForGlyphs: known tools map to their glyph; an unknown tool falls back
// to the muted default + bullet.
func TestAccentForGlyphs(t *testing.T) {
	cases := []struct {
		tool string
		want string
	}{
		{"run_command", "⌘"},
		{"edit_file", "✎"},
		{"read_file", "👁"},
		{"unknown_tool", "•"},
	}
	for _, tc := range cases {
		if got := accentFor(tc.tool).glyph; got != tc.want {
			t.Errorf("accentFor(%q).glyph = %q, want %q", tc.tool, got, tc.want)
		}
	}
	// The default uses the muted style (same foreground as muted).
	if accentFor("unknown_tool").style.GetForeground() != muted.GetForeground() {
		t.Errorf("unknown tool should default to the muted style")
	}
}

// TestVerifyGlyphs: the pass/fail glyphs are the expected ✓ / ✗ and their styles
// render their content.
func TestVerifyGlyphs(t *testing.T) {
	if glyphPass != "✓" || glyphFail != "✗" {
		t.Fatalf("verify glyphs = %q/%q, want ✓/✗", glyphPass, glyphFail)
	}
	if got := verifyPass.Render(glyphPass); !strings.Contains(got, "✓") {
		t.Errorf("verifyPass dropped its glyph: %q", got)
	}
	if got := verifyFail.Render(glyphFail); !strings.Contains(got, "✗") {
		t.Errorf("verifyFail dropped its glyph: %q", got)
	}
}

// TestPaletteColourCodes: each semantic style is built from its palette colour
// var, and those vars hold the exact codes the migration relied on (212/2/1/3/
// 244) — so a future retune is a deliberate, golden-affecting change rather than
// an accident. (The colour codes are pinned as plain strings so this test, like
// every non-theme file, contains no lipgloss.Color literal.)
func TestPaletteColourCodes(t *testing.T) {
	cases := []struct {
		name  string
		style lipgloss.Style
		color lipgloss.Color
		code  string
	}{
		{"accent", accent, accentColor, "212"},
		{"success", success, successColor, "2"},
		{"danger", danger, dangerColor, "1"},
		{"warning", warning, warningColor, "3"},
		{"muted", muted, mutedColor, "244"},
	}
	for _, tc := range cases {
		if got := tc.style.GetForeground(); got != tc.color {
			t.Errorf("%s style foreground = %v, want its palette colour var", tc.name, got)
		}
		if string(tc.color) != tc.code {
			t.Errorf("%s colour = %q, want %q (retune must be deliberate)", tc.name, string(tc.color), tc.code)
		}
	}
}
