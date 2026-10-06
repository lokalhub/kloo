package tui

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/tools"
)

// capturingClient records the request the loop actually sends, which is the only
// place the question "does the model receive the image?" can honestly be answered.
type capturingClient struct{ req *llm.ChatRequest }

func (c *capturingClient) Complete(_ context.Context, r llm.ChatRequest) (llm.ChatResponse, error) {
	if c.req == nil {
		cp := r
		c.req = &cp
	}
	return llm.ChatResponse{Choices: []llm.Choice{{Message: llm.Message{Role: llm.RoleAssistant, Content: "it is a screenshot"}}}}, nil
}

func (c *capturingClient) Stream(ctx context.Context, r llm.ChatRequest, _ func(llm.Delta) error) (llm.ChatResponse, error) {
	return c.Complete(ctx, r)
}

// TestPastedImageReachesTheModelAsAnImage is the end-to-end claim: paste a
// screenshot, submit, and the request on the wire carries it as an image_url
// content part on the task message.
//
// Every earlier step of this pipeline can be green while the feature does nothing
// — a placeholder in the input box looks identical whether the bytes travel or
// not. This test fails if ANY link breaks: the attachment, takeImages, the
// Runner's images argument, Loop.TaskImages, or the content-part marshalling.
func TestPastedImageReachesTheModelAsAnImage(t *testing.T) {
	const img = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUg=="

	root := t.TempDir()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{MaxSteps: 2, MaxContextTokens: 8000, ChurnRounds: 3}
	client := &capturingClient{}
	loop := &agent.Loop{
		Client:        client,
		Adapter:       tools.NativeFCAdapter{},
		Registry:      tools.NewRegistry(),
		Budget:        agent.NewBudget(cfg, nil),
		Churn:         agent.NewChurnDetector(3),
		Root:          root,
		ContextTokens: 8000,
		System:        "you are kloo",
	}
	runner := NewLoopRunner(loop, ws, 0)
	runner.setSend(func(tea.Msg) {})

	// Drive the real TUI path: paste the image, type a question, submit.
	m := newSized()
	m.runner = runner
	m, handled := m.handleImagePaste(img)
	if !handled {
		t.Fatal("the paste was not recognised as an image")
	}
	// submitTask launches the run in a goroutine, which would race the assertion, so
	// the test performs the same two steps it does and drives the runner directly.
	line := "what is in this? " + m.images[0].placeholder
	task, images := m.takeImages(m.expandPastes(line))
	rt := RuntimeConfig{
		Endpoint: "https://example.test/v1", Model: "vision-model", ContextTokens: 8000, ToolFormat: "native",
		NewClient: func(string, string, string) llm.LLMClient { return client },
	}
	runner.Start(context.Background(), task, rt, ModeAuto, nil, images)

	if client.req == nil {
		t.Fatal("the loop never sent a request")
	}
	body, err := json.Marshal(client.req.Messages)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"type":"image_url"`) {
		t.Fatalf("the request carries no image part:\n%s", body)
	}
	if !strings.Contains(string(body), img) {
		t.Fatalf("the request does not carry the pasted image:\n%s", body)
	}
	// And it must be an ATTACHMENT, never the text of the prompt.
	for _, msg := range client.req.Messages {
		if strings.Contains(msg.Content, "data:image/") {
			t.Errorf("the image leaked into message text (role %s): %q", msg.Role, msg.Content)
		}
	}
}

// TestNextRunDropsTheLastRunsImages: attachments are per-submission. A screenshot
// from the previous question riding along silently is both wrong and expensive.
func TestNextRunDropsTheLastRunsImages(t *testing.T) {
	root := t.TempDir()
	ws, _ := tools.NewWorkspace(root)
	cfg := config.Config{MaxSteps: 2, MaxContextTokens: 8000, ChurnRounds: 3}
	loop := &agent.Loop{
		Client:        &capturingClient{},
		Adapter:       tools.NativeFCAdapter{},
		Registry:      tools.NewRegistry(),
		Budget:        agent.NewBudget(cfg, nil),
		Churn:         agent.NewChurnDetector(3),
		Root:          root,
		ContextTokens: 8000,
		System:        "you are kloo",
	}
	runner := NewLoopRunner(loop, ws, 0)
	runner.setSend(func(tea.Msg) {})

	runner.Start(context.Background(), "first", RuntimeConfig{}, ModeAuto, nil, []string{"data:image/png;base64,AAA"})
	runner.Start(context.Background(), "second", RuntimeConfig{}, ModeAuto, nil, nil)
	if len(loop.TaskImages) != 0 {
		t.Errorf("the previous run's images survived into the next: %v", loop.TaskImages)
	}
}
