// brand.go renders kloo's own identity in the terminal: the splash wordmark shown
// on a fresh session and the `k>` composer prompt.
//
// The logo is a stem plus a caret — the letter k and the shell prompt in one glyph
// (docs/brand/). Only the "k" carries the brand amber; the rest stays in the
// terminal's default foreground so the banner sits inside the user's palette
// instead of fighting it.
package tui

import "strings"

// brandWordmark is the box-drawing form of "kloo". Box-drawing needs a UTF-8
// locale, so callers gate on localeSupportsUTF8 and fall back to brandASCII.
var brandWordmark = []string{
	"┬┌─ ┬   ┌─┐ ┌─┐",
	"├┴┐ │   │ │ │ │",
	"┴ ┴ ┴─┘ └─┘ └─┘",
}

// brandKRunes is how many leading runes of each wordmark line form the "k" — the
// slice that gets the brand colour, mirroring the amber arm in the SVG mark.
const brandKRunes = 3

// brandASCII is the no-UTF-8 fallback: the same mark as plain ASCII. It is also
// the form used anywhere a single line is all there is room for.
const brandASCII = "k>"

// brandTagline is the one-line description under the splash wordmark.
const brandTagline = "autonomous coding for small local models"

// composerPrompt is the input prompt. "k>" is the mark itself, so the thing you
// type at is the logo.
const composerPrompt = "k> "

// renderBrandBanner builds the fresh-session splash: the wordmark with its "k" in
// brand amber, then a dim tagline carrying the build version. With utf8 false it
// degrades to a single ASCII line rather than printing mojibake.
func renderBrandBanner(version string, utf8 bool) string {
	tagline := brandTagline
	if version != "" {
		tagline += " · " + version
	}
	if !utf8 {
		return brand.Render(brandASCII) + " kloo — " + muted.Render(tagline)
	}
	var b strings.Builder
	for _, line := range brandWordmark {
		r := []rune(line)
		b.WriteString(brand.Render(string(r[:brandKRunes])))
		b.WriteString(string(r[brandKRunes:]))
		b.WriteString("\n")
	}
	b.WriteString(muted.Render(tagline))
	return b.String()
}

// localeSupportsUTF8 reports whether the environment declares a UTF-8 locale, in
// POSIX precedence order (LC_ALL, then LC_CTYPE, then LANG): the FIRST variable
// that is set decides, because a set-but-non-UTF-8 LC_ALL overrides a UTF-8 LANG.
// Nothing set at all ⇒ false, so an unconfigured environment gets plain ASCII
// rather than a screen of replacement characters.
func localeSupportsUTF8(getenv func(string) string) bool {
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		v := getenv(key)
		if v == "" {
			continue
		}
		v = strings.ToLower(v)
		return strings.Contains(v, "utf-8") || strings.Contains(v, "utf8")
	}
	return false
}
