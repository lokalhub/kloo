package tui

import (
	"fmt"
	"strings"
)

// pasteInlineMax: pastes longer than this many bytes (or with any newline) collapse
// to a short placeholder in the input instead of flooding the line; the full text is
// kept and expanded on submit. Short single-line pastes insert normally.
const pasteInlineMax = 200

// imagePasteMax bounds an attached image, measured on the data URL (base64 is ~4/3
// of the file). Larger images are refused with a notice rather than silently
// dropped. 5 MiB of base64 is a generous full-screen screenshot; past that the
// request body alone starts to be the problem.
const imagePasteMax = 5 << 20 // 5 MiB

// pastedImage pairs the short placeholder shown in the input with the image's data
// URL, which is sent as a VISION CONTENT PART and never as message text.
//
// It is a separate type from pastedText for exactly that reason: expandPastes
// substitutes pasted text back into the prompt string, and doing that to an image
// would inline a megabyte of base64 into the conversation, where it would be
// tokenized, summarised by the compactor and re-sent every turn.
type pastedImage struct{ placeholder, dataURL string }

// pastedText pairs the short placeholder shown in the input with the full pasted
// text sent to the model on submit.
type pastedText struct{ placeholder, full string }

// handleImagePaste accepts an image that arrived as PASTED TEXT: a data URL, or a
// path to an image file (what a terminal delivers when a file is dragged onto it).
// Returns (model, handled); handled=false means it was not an image and the caller
// should fall through to the text-paste path.
//
// The clipboard holding an actual PNG is the third case, and it never reaches
// here: a terminal sends no paste event for it at all. That one is ctrl+v
// (pasteClipboardImage).
func (m Model) handleImagePaste(text string) (Model, bool) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "data:image/") {
		return m.attachImage(trimmed), true
	}
	if url, ok := imageFileFromPaste(trimmed); ok {
		return m.attachImage(url), true
	}
	return m, false
}

// takeImages returns the attached images' data URLs and strips their placeholders
// from the submitted line, leaving the prompt as the words the user actually typed.
//
// Stripping matters: "[#1 image png 410.2k]" means nothing to the model, and
// leaving it in invites it to reason about a filename that does not exist.
func (m Model) takeImages(line string) (string, []string) {
	if len(m.images) == 0 {
		return line, nil
	}
	urls := make([]string, 0, len(m.images))
	for _, img := range m.images {
		if !strings.Contains(line, img.placeholder) {
			continue // the user deleted the placeholder: they removed the attachment
		}
		line = strings.ReplaceAll(line, img.placeholder, "")
		urls = append(urls, img.dataURL)
	}
	return strings.TrimSpace(strings.Join(strings.Fields(line), " ")), urls
}

// handlePaste collapses a long/multi-line bracketed paste into a placeholder (like
// "[#1 pasted 320 lines, 9.1k chars]") appended to the input, stashing the full text.
// Returns (model, handled): handled=false means it's a short paste the input should
// insert as-is. Mirrors how Claude Code / Codex show "[Pasted text …]".
func (m Model) handlePaste(text string) (Model, bool) {
	if len(text) <= pasteInlineMax && !strings.Contains(text, "\n") {
		return m, false // short single-line paste → let the input insert it
	}
	lines := strings.Count(text, "\n") + 1
	ph := fmt.Sprintf("[#%d pasted %d lines, %s chars]", len(m.pastes)+1, lines, human(len(text)))
	m.pastes = append(m.pastes, pastedText{placeholder: ph, full: text})
	m.input.SetValue(m.input.Value() + ph)
	return m, true
}

// expandPastes replaces each paste placeholder in line with its full text (what the
// model receives); the transcript keeps the short placeholder.
func (m Model) expandPastes(line string) string {
	for _, p := range m.pastes {
		line = strings.ReplaceAll(line, p.placeholder, p.full)
	}
	return line
}
