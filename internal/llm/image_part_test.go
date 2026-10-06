package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestImageMessageSerializesAsContentParts: an attached image rides in an
// image_url content part, NOT in the message text.
//
// The first cut of image paste stuffed the whole base64 data URL into Content. A
// model reads that as gibberish, the tokenizer charges for every character of it,
// and the working set re-sends it on every turn — a 1 MiB screenshot is roughly
// 350k tokens of noise, which is more than the entire context window kloo targets.
func TestImageMessageSerializesAsContentParts(t *testing.T) {
	const img = "data:image/png;base64,iVBORw0KGgo="
	b, err := json.Marshal(Message{Role: RoleUser, Content: "what is in this screenshot?", Images: []string{img}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Role    string `json:"role"`
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			ImageURL *struct {
				URL string `json:"url"`
			} `json:"image_url"`
		} `json:"content"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("content is not a parts array: %s", b)
	}
	if len(got.Content) != 2 {
		t.Fatalf("want text + image parts, got %d: %s", len(got.Content), b)
	}
	if got.Content[0].Type != "text" || got.Content[0].Text != "what is in this screenshot?" {
		t.Errorf("part 0 should be the prompt text: %s", b)
	}
	if got.Content[1].Type != "image_url" || got.Content[1].ImageURL == nil || got.Content[1].ImageURL.URL != img {
		t.Errorf("part 1 should be the image_url: %s", b)
	}
	if strings.Contains(got.Content[0].Text, "base64") {
		t.Error("the image leaked into the text part")
	}
}

// TestNoImagesSerializesUnchanged: the plain shape is the compatibility guarantee —
// a message with no images and no cache breakpoint must serialize exactly as it did
// before images existed, so every endpoint that has never heard of vision is
// untouched.
func TestNoImagesSerializesUnchanged(t *testing.T) {
	b, err := json.Marshal(Message{Role: RoleUser, Content: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"role":"user","content":"hello"}` {
		t.Errorf("plain message changed shape: %s", b)
	}
}

// TestNormalizeKeepsImagesWhenMergingMessages: merging two same-role messages must
// not destroy the attachments of either.
//
// This is the bug that survived every other test. kloo's working memory and session
// history both end in a user message, and the new task message is also role=user, so
// on the SECOND turn of a session the two merge here — and a merge that copied only
// Content and ToolCalls threw the image away. The FIRST turn worked (nothing to merge
// with), so the pipeline looked correct right up to the point a user asked a second
// question.
func TestNormalizeKeepsImagesWhenMergingMessages(t *testing.T) {
	const img = "data:image/png;base64,iVBORw0KGgo="
	out := normalizeMessages([]Message{
		{Role: RoleUser, Content: "earlier question"},
		{Role: RoleUser, Content: "what is in this screenshot?", Images: []string{img}},
	})
	if len(out) != 1 {
		t.Fatalf("expected the two user messages to merge, got %d", len(out))
	}
	if len(out[0].Images) != 1 || out[0].Images[0] != img {
		t.Fatalf("the merge dropped the attachment: %+v", out[0].Images)
	}
	if !strings.Contains(out[0].Content, "earlier question") || !strings.Contains(out[0].Content, "screenshot") {
		t.Errorf("merged content lost text: %q", out[0].Content)
	}

	// And images on BOTH sides survive, in order.
	out2 := normalizeMessages([]Message{
		{Role: RoleUser, Content: "first", Images: []string{"a"}},
		{Role: RoleUser, Content: "second", Images: []string{"b"}},
	})
	if len(out2) != 1 || len(out2[0].Images) != 2 || out2[0].Images[0] != "a" || out2[0].Images[1] != "b" {
		t.Errorf("both sides' images must survive in order: %+v", out2)
	}
}
