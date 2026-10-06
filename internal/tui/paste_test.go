package tui

import (
	"strings"
	"testing"
)

func TestHandleImagePaste(t *testing.T) {
	// A data-URL image paste becomes an ATTACHMENT, not pasted text.
	img := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="
	m, handled := newSized().handleImagePaste(img)
	if !handled {
		t.Fatal("image paste should be handled")
	}
	if len(m.images) != 1 || m.images[0].dataURL != img {
		t.Fatalf("image not attached: %+v", m.images)
	}
	if len(m.pastes) != 0 {
		t.Errorf("an image must not be stashed as pasted TEXT: %+v", m.pastes)
	}
	if !strings.Contains(m.input.Value(), "[#1 image png") {
		t.Errorf("placeholder not inserted into input: %q", m.input.Value())
	}

	// Non-image text falls through to the text-paste path.
	if _, handled := newSized().handleImagePaste("just text"); handled {
		t.Errorf("plain text should not be treated as an image paste")
	}
}

// TestImageNeverEntersThePromptText is the whole point of attaching images
// separately. Substituting the data URL into the prompt (what expandPastes does
// for TEXT) sends ~1.4 MiB of base64 as message content: the model reads noise,
// the tokenizer charges for all of it, and the working set re-sends it every
// single turn. takeImages must hand the URL over as an attachment and leave the
// typed words behind.
func TestImageNeverEntersThePromptText(t *testing.T) {
	img := "data:image/png;base64," + strings.Repeat("A", 4096)
	m, _ := newSized().handleImagePaste(img)
	line := "what is wrong here? " + m.images[0].placeholder

	task, urls := m.takeImages(line)
	if len(urls) != 1 || urls[0] != img {
		t.Fatalf("image not handed over as an attachment: %d urls", len(urls))
	}
	if strings.Contains(task, "base64") || strings.Contains(task, "data:image") {
		t.Errorf("the image leaked into the prompt text: %q", task)
	}
	if strings.Contains(task, "[#1 image") {
		t.Errorf("the placeholder is meaningless to the model and must be stripped: %q", task)
	}
	if task != "what is wrong here?" {
		t.Errorf("task = %q, want the typed words alone", task)
	}
}

// TestDeletedPlaceholderDropsTheAttachment: the placeholder in the input IS the
// attachment. Backspacing it away is how a user cancels an image, so a URL whose
// placeholder is gone must not be sent anyway.
func TestDeletedPlaceholderDropsTheAttachment(t *testing.T) {
	m, _ := newSized().handleImagePaste("data:image/png;base64,iVBORw0KGgo=")
	_, urls := m.takeImages("never mind")
	if len(urls) != 0 {
		t.Errorf("a removed placeholder must drop its attachment, got %d", len(urls))
	}
}

func TestHandlePaste(t *testing.T) {
	// Short single-line paste is NOT collapsed — the input inserts it as-is.
	if _, handled := newSized().handlePaste("a short line"); handled {
		t.Errorf("short single-line paste should not collapse to a placeholder")
	}

	// Long/multi-line paste collapses to a placeholder; the full text is stashed
	// and expandPastes restores it for the model.
	long := strings.Repeat("x", 50) + "\n" + strings.Repeat("y", 50) + "\nthird line"
	m, handled := newSized().handlePaste(long)
	if !handled {
		t.Fatal("multi-line paste should collapse to a placeholder")
	}
	if len(m.pastes) != 1 || m.pastes[0].full != long {
		t.Fatalf("full paste not stashed: %+v", m.pastes)
	}
	if !strings.Contains(m.input.Value(), "[#1 pasted 3 lines") {
		t.Errorf("placeholder not inserted into input: %q", m.input.Value())
	}
	if got := m.expandPastes(m.input.Value()); got != long {
		t.Errorf("expandPastes did not restore the full text:\n got %q\nwant %q", got, long)
	}
}
