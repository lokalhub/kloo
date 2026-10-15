package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/tools"
)

// The point of the whole vision feature: an image a TOOL produced has to reach the
// WIRE as a vision content part. This asserts the serialized message, because every
// layer in between (tools.Result → observation → llm.Message → MarshalJSON) is a
// place the attachment could be silently dropped — which is exactly how it behaved
// before: Message.Images was only ever populated from the human's pasted
// TaskImages, so a screenshot the agent took itself was invisible to it.
func TestObservationCarriesToolImagesAsVisionParts(t *testing.T) {
	res := tools.Result{
		Output: "image shot.png (image/png, 70 bytes) attached below — look at it.",
		Images: []string{"data:image/png;base64,AAAA"},
	}
	raw, err := json.Marshal(observation(tools.Call{Name: tools.NameViewImage}, res, nil))
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
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("content is not the parts form, so no image was attached: %v — body %s", err, raw)
	}
	if got.Role != "user" {
		t.Errorf("role = %q, want user: several OpenAI-compatible endpoints reject image parts on a tool-result message", got.Role)
	}
	if len(got.Content) != 2 || got.Content[0].Type != "text" || got.Content[1].Type != "image_url" {
		t.Fatalf("want [text, image_url], got %s", raw)
	}
	if !strings.Contains(got.Content[0].Text, "shot.png") {
		t.Errorf("the text part lost the tool output: %q", got.Content[0].Text)
	}
	if got.Content[1].ImageURL.URL != "data:image/png;base64,AAAA" {
		t.Errorf("image URL = %q", got.Content[1].ImageURL.URL)
	}
}

// A result with no images must serialize byte-identically to what it always did —
// the plain string-content form. Anything else would change every request body in
// the product for a feature only one tool uses.
func TestObservationWithoutImagesIsUnchanged(t *testing.T) {
	raw, err := json.Marshal(observation(tools.Call{Name: tools.NameReadFile}, tools.Result{Output: "hello"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"content":"tool read_file result:\nhello"`) {
		t.Errorf("non-image observation changed shape: %s", raw)
	}
}
