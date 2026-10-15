package agent

import (
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
)

func imgMsg(text string, n int) llm.Message {
	urls := make([]string, n)
	for i := range urls {
		urls[i] = "data:image/png;base64,AAAA"
	}
	return llm.Message{Role: llm.RoleUser, Content: text, Images: urls}
}

// The live failure: a run looked at three screenshots of a real app and every
// request after that re-uploaded all three. The attachments have to be bounded
// or the request body only grows.
func TestWindowImagesKeepsOnlyTheLastTwo(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "task"},
		imgMsg("image wide-login.png attached", 1),
		{Role: llm.RoleAssistant, Content: "looking"},
		imgMsg("image phone-login.png attached", 1),
		imgMsg("image wide-lobby.png attached", 1),
	}
	out := windowImages(msgs)

	var live int
	for _, m := range out {
		live += len(m.Images)
	}
	if live != maxLiveImages {
		t.Errorf("live attachments = %d, want %d", live, maxLiveImages)
	}
	if len(out[1].Images) != 0 {
		t.Error("the OLDEST image should have been detached")
	}
	if len(out[3].Images) != 1 || len(out[4].Images) != 1 {
		t.Error("the two most recent images must still be attached")
	}
	// The note matters as much as the dropping: a silently vanished image leaves
	// the model reasoning about a picture that is no longer in front of it.
	if !strings.Contains(out[1].Content, "detached") || !strings.Contains(out[1].Content, "view_image") {
		t.Errorf("the detached message does not say what happened: %q", out[1].Content)
	}
	if !strings.Contains(out[1].Content, "wide-login.png") {
		t.Errorf("the detached message lost the original text naming the file: %q", out[1].Content)
	}
}

// Under the cap nothing is touched — a two-screenshot before/after comparison is
// the common case and must arrive whole.
func TestWindowImagesLeavesASmallRunAlone(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "task"},
		imgMsg("before.png", 1),
		imgMsg("after.png", 1),
	}
	out := windowImages(msgs)
	for i, m := range out {
		if len(m.Images) != len(msgs[i].Images) || m.Content != msgs[i].Content {
			t.Fatalf("message %d was modified: %+v", i, m)
		}
	}
}

// A run with no images at all must be byte-identical: this runs on EVERY request
// in the product, including every text-only one.
func TestWindowImagesIsANoOpWithoutImages(t *testing.T) {
	msgs := []llm.Message{
		{Role: llm.RoleUser, Content: "task"},
		{Role: llm.RoleAssistant, Content: "ok"},
	}
	out := windowImages(msgs)
	if &out[0] != &msgs[0] {
		// Not an aliasing requirement, but the contents must match exactly.
		for i := range msgs {
			if out[i].Content != msgs[i].Content || len(out[i].Images) != 0 {
				t.Fatalf("message %d changed: %+v", i, out[i])
			}
		}
	}
}

// One message carrying several images counts them all, and the note says how many.
func TestWindowImagesCountsPerAttachmentNotPerMessage(t *testing.T) {
	msgs := []llm.Message{
		imgMsg("three at once", 3),
		imgMsg("latest", 1),
	}
	out := windowImages(msgs)
	var live int
	for _, m := range out {
		live += len(m.Images)
	}
	if live != 1 {
		t.Errorf("live attachments = %d, want 1 (the 3-image message is older and goes whole)", live)
	}
	if !strings.Contains(out[0].Content, "3 earlier images") {
		t.Errorf("the note does not say how many were detached: %q", out[0].Content)
	}
}
