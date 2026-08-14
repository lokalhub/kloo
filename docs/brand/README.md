# kloo brand

The mark is a stem and a caret: the letter **k** and the shell prompt **>**, fused
into one glyph. It says "terminal-native" before it says anything else, which is
the correct first impression for kloo.

## Files

| File | Use |
|---|---|
| `mark.svg` | The mark alone. Transparent, stem uses `currentColor`. Docs, embeds, anywhere you control the surrounding colour. |
| `wordmark.svg` | Mark + "kloo" as outlined letterforms. Anywhere with horizontal room: README headers, docs, slides. |
| `favicon.svg` | Fixed-colour mark on an ink tile. Browser tabs, OS docks, repo avatars. |
| `favicon-16/32/48.png`, `apple-touch-icon.png`, `icon-512.png` | Rasterised favicons. |
| `mark-on-{light,dark}.png`, `wordmark-on-{light,dark}.png` | Rasterised transparent PNGs, 2× for retina. |

Regenerate the PNGs from the SVGs with headless Chrome — there is no build step
and no committed toolchain; the SVGs are the source of truth.

## Colour

| Token | Value | Use |
|---|---|---|
| Amber | `#F2A03D` | The caret. On dark backgrounds. |
| Amber (deep) | `#C8762A` | The caret on light backgrounds — the bright amber fails contrast on paper. |
| Ink | `#14151A` | The stem on light backgrounds; the favicon tile. |
| Paper | `#FAF9F7` | The stem on dark backgrounds. |

Amber is deliberate. Go tooling defaults to cyan and AI tooling defaults to violet;
kloo is neither, and a warm accent reads as a lamp left on — something running
quietly on your own machine.

In the TUI the amber is 256-colour **215**, defined once as `brandColor` in
`internal/tui/theme.go`. It is kept distinct from `accentColor` (the UI's tool-card
accent) on purpose: brand amber marks kloo's own identity — the splash and the
`k>` prompt — and nothing else.

## Rules

- **One accent element.** The caret is amber; everything else is neutral. Never
  colour the whole mark.
- **Clear space** of at least the stem's height (8 units in the 64-unit viewBox) on
  all sides.
- **Minimum size** 16 px for the mark, 96 px wide for the wordmark. Below that, use
  the ASCII form.
- **Don't** rotate, skew, recolour the caret, add gradients or shadows, or
  reconstruct the wordmark with a system font.

## The mark vs. the wordmark's k

They differ, on purpose. In `mark.svg` the caret is **detached** and centred in the
square, so the glyph reads as both a `k` and a prompt. In `wordmark.svg` the arm
**joins** the stem and sits on the x-height, so the word reads as "kloo".

A mark has to signify; a wordmark has to spell.

## ASCII

kloo's logo is seen in a terminal far more often than on a page, so it has to
degrade to text. `internal/tui/brand.go` owns these.

```
┬┌─ ┬   ┌─┐ ┌─┐        the splash wordmark; the "k" carries the amber
├┴┐ │   │ │ │ │
┴ ┴ ┴─┘ └─┘ └─┘

k>                     the mark itself — also the composer prompt
```

Box-drawing needs a UTF-8 locale. `localeSupportsUTF8` checks `LC_ALL`, `LC_CTYPE`,
`LANG` in POSIX precedence and falls back to the plain `k>` line when the
environment doesn't declare one, so an unconfigured terminal gets ASCII rather than
a screen of replacement characters.

Full concept sheet, including the four rejected directions and why:
`lokal/docs/apps/kloo/plans/logos/index.html`.
