package agent

import (
	"fmt"
	"strings"

	"github.com/lokalhub/kloo/internal/llm"
)

// maxLiveImages is how many image attachments the assembled request may carry.
//
// Two, not one: a visual task is almost always a comparison — the screenshot
// before a change and the screenshot after it — and dropping to one makes that
// impossible to see in a single turn.
const maxLiveImages = 2

// windowImages keeps only the LAST maxLiveImages attachments in the conversation
// and replaces every older one with a one-line note naming what used to be there.
//
// Why this exists: llm.Message.Images carries an image as a vision content part
// precisely so it is not inlined into the text, where it "would be tokenized,
// summarised by the compactor and re-sent every turn". But keeping it on the
// message has exactly that last effect anyway — the message stays in the
// conversation, so every subsequent request re-uploads every screenshot the run
// has ever looked at. Three screenshots of a real app is a ~240 KB request body,
// and it only grows.
//
// The working-set compactor cannot help: it reasons about text, and an image
// contributes almost nothing to its token estimate (two screenshots measured
// 1,595 prompt tokens) while dominating the bytes on the wire. So the bound has
// to be on the attachments themselves.
//
// The note matters as much as the dropping. A silently vanished image leaves the
// model reasoning about a picture that is no longer in front of it; a note tells
// it the picture existed, what it was, and that it can look again.
func windowImages(msgs []llm.Message) []llm.Message {
	// Walk BACKWARDS counting attachments, not messages: one message can carry
	// several images, and counting messages let four attachments through a cap of
	// two. Caught by TestWindowImagesCountsPerAttachmentNotPerMessage.
	//
	// A message is kept whole or detached whole. Splitting one would renumber the
	// images the model has already been referring to.
	cut := -1 // index below which everything is detached
	live := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		n := len(msgs[i].Images)
		if n == 0 {
			continue
		}
		if live+n > maxLiveImages {
			cut = i
			break
		}
		live += n
	}
	if cut < 0 {
		return msgs // nothing to shed: leave the slice untouched
	}

	out := make([]llm.Message, len(msgs))
	copy(out, msgs)
	for i := 0; i <= cut; i++ {
		n := len(out[i].Images)
		if n == 0 {
			continue
		}
		out[i].Images = nil
		out[i].Content = strings.TrimRight(out[i].Content, "\n") +
			fmt.Sprintf("\n[%s detached to keep the request small — call view_image again to look at it.]", imagesPhrase(n))
	}
	return out
}

func imagesPhrase(n int) string {
	if n == 1 {
		return "1 earlier image"
	}
	return fmt.Sprintf("%d earlier images", n)
}
