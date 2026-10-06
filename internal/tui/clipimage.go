package tui

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Reading an IMAGE out of the clipboard needs a helper process, and that is not an
// implementation detail we could avoid — it is the whole reason "paste an image"
// did not work before.
//
// A terminal's bracketed paste carries TEXT. When the clipboard holds a PNG, a
// terminal emits nothing at all on Ctrl+V: there is no paste event to intercept,
// so code that waits for a "data:image/…" paste to arrive waits forever. (That is
// why the first attempt at this could not be tested by hand — the path it
// implemented is one no terminal can take.) kloo therefore reads the clipboard
// itself, on an explicit key, through whichever of these the system has.
var clipboardImageReaders = []clipReader{
	// Wayland. --no-newline matters: wl-paste appends one, which corrupts the PNG.
	{"wl-paste", []string{"--no-newline", "--type", "image/png"}, "image/png"},
	// X11.
	{"xclip", []string{"-selection", "clipboard", "-t", "image/png", "-o"}, "image/png"},
	{"xsel", []string{"--clipboard", "--output"}, "image/png"},
	// macOS (pngpaste is the common Homebrew helper; "-" writes to stdout).
	{"pngpaste", []string{"-"}, "image/png"},
}

type clipReader struct {
	bin  string
	args []string
	mime string
}

// clipboardReadTimeout bounds a helper that hangs (wl-paste with no compositor,
// xclip with no X server) so a keypress can never freeze the TUI.
const clipboardReadTimeout = 3 * time.Second

// errNoClipboardImage is "there is no image on the clipboard", as distinct from
// "no tool is installed to look" — the two need different advice.
var errNoClipboardImage = errors.New("no image on the clipboard")

// errNoClipboardTool is "nothing on this system can read the clipboard".
var errNoClipboardTool = errors.New("no clipboard tool")

// readClipboardImage returns the clipboard's image as a data URL. It tries each
// known helper in turn and reports the two failures separately, because the fix
// differs: install wl-clipboard/xclip, versus copy an image first.
func readClipboardImage() (string, error) {
	found := false
	for _, r := range clipboardImageReaders {
		if _, err := exec.LookPath(r.bin); err != nil {
			continue
		}
		found = true
		ctx, cancel := context.WithTimeout(context.Background(), clipboardReadTimeout)
		out, err := exec.CommandContext(ctx, r.bin, r.args...).Output()
		cancel()
		if err != nil || len(out) == 0 {
			continue // this selection holds no image; try the next tool
		}
		mime, ok := sniffImage(out)
		if !ok {
			continue // the clipboard held something, but not an image
		}
		return dataURL(mime, out), nil
	}
	if !found {
		return "", errNoClipboardTool
	}
	return "", errNoClipboardImage
}

// sniffImage identifies an image by its magic bytes and returns its MIME type.
// The bytes decide, not the helper's promise or a file extension: a helper asked
// for image/png can hand back an error page, and a file named .png can be a JPEG.
func sniffImage(b []byte) (string, bool) {
	switch {
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png", true
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg", true
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif", true
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp", true
	}
	return "", false
}

// dataURL renders image bytes as the data URL the vision content part carries.
func dataURL(mime string, b []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
}

// imageFileFromPaste reads an image that was pasted as a FILE PATH. Dragging a
// file onto a terminal, and several screenshot tools' "copy path" action, deliver
// the path as text — so this is the second real way an image arrives, and it costs
// one stat to support.
//
// Returns ("", false) for anything that is not a single readable image path, so
// ordinary text still falls through to the text-paste handler.
func imageFileFromPaste(text string) (string, bool) {
	p := strings.TrimSpace(text)
	// A dragged path is often quoted, and may be escaped by the terminal.
	p = strings.Trim(p, `"'`)
	p = strings.ReplaceAll(p, `\ `, " ")
	p = strings.TrimPrefix(p, "file://")
	if p == "" || strings.ContainsAny(p, "\n\r") {
		return "", false
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			p = filepath.Join(home, p[2:])
		}
	}
	if !filepath.IsAbs(p) {
		return "", false // a bare word is text, not a drag-and-drop
	}
	st, err := os.Stat(p)
	if err != nil || st.IsDir() || st.Size() == 0 || st.Size() > imagePasteMax {
		return "", false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false
	}
	mime, ok := sniffImage(b)
	if !ok {
		return "", false
	}
	return dataURL(mime, b), true
}

// attachImage stashes a data URL as an attachment and puts a short placeholder in
// the input. The placeholder is what the TRANSCRIPT shows and what the user edits;
// the pixels never go near the text (see pastedImage).
func (m Model) attachImage(url string) Model {
	if len(url) > imagePasteMax {
		return m.appendItem(infoItem{text: fmt.Sprintf(
			"image too large (%s); max %s — crop or scale it down", human(len(url)), human(imagePasteMax))})
	}
	ph := fmt.Sprintf("[#%d image %s %s]", len(m.images)+1, imageKind(url), human(len(url)))
	m.images = append(m.images, pastedImage{placeholder: ph, dataURL: url})
	// Trailing space, because attaching usually comes BEFORE typing the question:
	// without it the next keystroke lands flush against the placeholder and the input
	// reads "[#1 image png 214.5k]do you see this image". Leading space only when
	// there is already text to separate from.
	cur := m.input.Value()
	if cur != "" && !strings.HasSuffix(cur, " ") {
		cur += " "
	}
	m.input.SetValue(cur + ph + " ")
	// SetValue does NOT move the cursor, so without this the next keystroke lands
	// wherever the cursor happened to be — after two attachments the input reads
	// "[#1 …] hello[#2 …]", with the typed words wedged between the placeholders.
	// Seen in a live session; the attachment still worked, but the input looked
	// broken enough to make you doubt it had.
	m.input.CursorEnd()
	return m
}

// imageKind is the short label in a placeholder ("png"), from the data URL's MIME.
func imageKind(url string) string {
	const p = "data:image/"
	if !strings.HasPrefix(url, p) {
		return "image"
	}
	rest := url[len(p):]
	if i := strings.IndexAny(rest, ";,"); i >= 0 {
		return rest[:i]
	}
	return "image"
}

// pasteClipboardImage handles the explicit "attach the clipboard image" key. Both
// failures are reported to the user: silence on a keypress is indistinguishable
// from a broken build, which is exactly how this feature failed the first time.
func (m Model) pasteClipboardImage() Model {
	url, err := readClipboardImage()
	switch {
	case errors.Is(err, errNoClipboardTool):
		return m.appendItem(infoItem{text: "can't read the clipboard — install wl-clipboard (Wayland), xclip (X11) or pngpaste (macOS)"})
	case errors.Is(err, errNoClipboardImage):
		return m.appendItem(infoItem{text: "no image on the clipboard — copy one first (ctrl+v attaches images; text pastes normally)"})
	case err != nil:
		return m.appendItem(infoItem{text: "couldn't read a clipboard image: " + oneLine(err.Error())})
	}
	return m.attachImage(url)
}
