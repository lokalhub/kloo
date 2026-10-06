package tui

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// pngBytes is a minimal valid-headered PNG: the magic plus some payload. Sniffing
// reads the header, so this is enough to exercise the real path.
func pngBytes() []byte {
	return append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0x42}, 64)...)
}

// TestSniffImageReadsTheBytes: the format comes from the file's magic, never from
// a filename or from which helper produced it. A helper asked for image/png can
// return an error page, and screenshot tools happily name JPEGs ".png".
func TestSniffImageReadsTheBytes(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
		ok   bool
	}{
		{"png", pngBytes(), "image/png", true},
		{"jpeg", []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00}, "image/jpeg", true},
		{"gif", []byte("GIF89a....."), "image/gif", true},
		{"webp", []byte("RIFF\x00\x00\x00\x00WEBPVP8 "), "image/webp", true},
		{"html error page", []byte("<html>no selection</html>"), "", false},
		{"empty", nil, "", false},
	}
	for _, c := range cases {
		got, ok := sniffImage(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: sniffImage = (%q, %v), want (%q, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// TestImageFileFromPaste: dragging a screenshot onto the terminal delivers its
// PATH as text. That is one of only two ways an image can arrive as a paste event,
// so it has to be read from disk rather than treated as a sentence.
func TestImageFileFromPaste(t *testing.T) {
	dir := t.TempDir()
	img := filepath.Join(dir, "Screenshot From 2026-10-06.png")
	if err := os.WriteFile(img, pngBytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	notImg := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(notImg, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A terminal may quote or backslash-escape the spaces in a dragged path, and
	// some send a file:// URL. All three are the same file.
	for _, in := range []string{img, `"` + img + `"`, strings.ReplaceAll(img, " ", `\ `), "file://" + img} {
		url, ok := imageFileFromPaste(in)
		if !ok {
			t.Errorf("dragged path %q not recognised as an image", in)
			continue
		}
		if !strings.HasPrefix(url, "data:image/png;base64,") {
			t.Errorf("%q produced %q, want a png data URL", in, url[:40])
		}
	}

	// Everything that is not a readable image falls through to the text handler.
	for _, in := range []string{notImg, filepath.Join(dir, "missing.png"), dir, "just a sentence", "./relative.png", ""} {
		if _, ok := imageFileFromPaste(in); ok {
			t.Errorf("%q should NOT be treated as an image paste", in)
		}
	}
}

// TestOversizeImageIsRefusedLoudly: an image past the cap must say so. Silently
// dropping it is the failure mode that makes a feature look broken rather than
// bounded — the user pastes, nothing appears, and there is nothing to read.
func TestOversizeImageIsRefusedLoudly(t *testing.T) {
	m := newSized().attachImage("data:image/png;base64," + strings.Repeat("A", imagePasteMax))
	if len(m.images) != 0 {
		t.Error("an oversize image must not be attached")
	}
	if !contains(m.View(), "image too large") {
		t.Errorf("the refusal must be visible to the user:\n%s", m.View())
	}
}

// TestImageKind labels the placeholder from the data URL's own MIME type.
func TestImageKind(t *testing.T) {
	for in, want := range map[string]string{
		"data:image/png;base64,AAA":  "png",
		"data:image/jpeg;base64,AAA": "jpeg",
		"data:image/webp,AAA":        "webp",
		"https://example.com/a.png":  "image",
	} {
		if got := imageKind(in); got != want {
			t.Errorf("imageKind(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPlaceholderSpacing: attaching usually happens BEFORE the question is typed, so
// the placeholder must leave a trailing space. Without it the input reads
// "[#1 image png 214.5k]do you see this image" — seen in a real tmux session.
func TestPlaceholderSpacing(t *testing.T) {
	const img = "data:image/png;base64,iVBORw0KGgo="

	// Attach first, then type: there must be a gap to type into.
	m := newSized().attachImage(img)
	if !strings.HasSuffix(m.input.Value(), " ") {
		t.Errorf("no trailing space to type after: %q", m.input.Value())
	}

	// Type first, then attach: exactly one space joins them, never two.
	m2 := newSized()
	m2.input.SetValue("explain this")
	m2 = m2.attachImage(img)
	if strings.Contains(m2.input.Value(), "  ") {
		t.Errorf("doubled space when attaching after text: %q", m2.input.Value())
	}
	if !strings.HasPrefix(m2.input.Value(), "explain this [#1 image png") {
		t.Errorf("placeholder not appended cleanly: %q", m2.input.Value())
	}
}

// TestTypingLandsAfterThePlaceholder: SetValue leaves the cursor where it was, so an
// attachment has to move it to the end. Without this, attaching twice and then typing
// produces "[#1 …] hello[#2 …]" — the words wedged between the placeholders.
func TestTypingLandsAfterThePlaceholder(t *testing.T) {
	m := newSized()
	m, _ = m.handleImagePaste("data:image/png;base64,AAAA")
	m, _ = m.handleImagePaste("data:image/png;base64,BBBB")

	var mm tea.Model = m
	for _, r := range "hello" {
		mm, _ = mm.(Model).Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	got := mm.(Model).input.Value()
	if !strings.HasSuffix(strings.TrimSpace(got), "hello") {
		t.Errorf("typing did not land after both placeholders: %q", got)
	}
}
