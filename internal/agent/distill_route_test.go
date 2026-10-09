package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// completion is a minimal non-streaming chat completion body.
const completion = `{"choices":[{"message":{"role":"assistant","content":"edited a.go; npm test FAILS"}}],"usage":{"total_tokens":42}}`

// TestDistillerDefaultsToTheRunsOwnModel pins the contract that makes the whole
// feature safe to ship: with nothing configured, the summariser calls the run's
// own client with the run's own model id and asks for the built-in word budget.
// An unset knob must be byte-identical to kloo before the knobs existed.
func TestDistillerDefaultsToTheRunsOwnModel(t *testing.T) {
	own := llmtest.JSON(t, completion)
	l := &Loop{
		Client:   llm.New(own.URL+"/v1", "own-model"),
		Model:    "own-model",
		Endpoint: own.URL + "/v1",
		// The brief's tokens are charged to the run, so a Budget is required to
		// call the closure at all (observeUsage).
		Budget: &stubBudget{tripAt: 1000},
	}
	d := l.distiller(context.Background())
	if d == nil {
		t.Fatal("distillation is on by default; distiller() returned nil")
	}
	if _, err := d([]string{"read a.go", "ran npm test: FAIL"}); err != nil {
		t.Fatalf("distill: %v", err)
	}
	calls := own.ModelCalls()
	if len(calls) != 1 {
		t.Fatalf("model calls = %d, want 1 on the run's own endpoint", len(calls))
	}
	body := string(calls[0])
	if !strings.Contains(body, `"own-model"`) {
		t.Errorf("the brief was not asked of the run's own model: %s", body)
	}
	if !strings.Contains(body, "at most 220 words") {
		t.Errorf("word budget is not the built-in 220: %s", body)
	}
}

// TestDistillerRoutesToTheConfiguredModel: --distill-model/--distill-endpoint send
// the brief somewhere else, and the run's own endpoint must not be touched — the
// point of the separate endpoint is to avoid a model swap on a single-GPU server,
// which a call to the main endpoint would defeat.
func TestDistillerRoutesToTheConfiguredModel(t *testing.T) {
	own := llmtest.JSON(t, completion)
	brief := llmtest.JSON(t, completion)
	var builtFor [2]string
	l := &Loop{
		Client:          llm.New(own.URL+"/v1", "own-model"),
		Model:           "own-model",
		Endpoint:        own.URL + "/v1",
		DistillModel:    "brief-model",
		DistillEndpoint: brief.URL + "/v1",
		DistillMaxWords: 400,
		Budget:          &stubBudget{tripAt: 1000},
		NewDistillClient: func(ep, model string) llm.LLMClient {
			builtFor = [2]string{ep, model}
			return llm.New(ep, model)
		},
	}
	d := l.distiller(context.Background())
	if _, err := d([]string{"read a.go"}); err != nil {
		t.Fatalf("distill: %v", err)
	}
	if builtFor != [2]string{brief.URL + "/v1", "brief-model"} {
		t.Errorf("client built for %v, want the distill endpoint+model", builtFor)
	}
	if n := len(own.ModelCalls()); n != 0 {
		t.Errorf("the run's own endpoint was called %d time(s); the route exists to avoid exactly that", n)
	}
	calls := brief.ModelCalls()
	if len(calls) != 1 {
		t.Fatalf("distill endpoint calls = %d, want 1", len(calls))
	}
	body := string(calls[0])
	if !strings.Contains(body, `"brief-model"`) {
		t.Errorf("wrong model on the distill call: %s", body)
	}
	if !strings.Contains(body, "at most 400 words") {
		t.Errorf("configured word budget not honoured: %s", body)
	}
	// The hard bound must grow with the ask, or a 400-word brief is cut off
	// mid-sentence by the 700-token floor.
	if got := distillTokens(400); got <= distillMaxTokens {
		t.Errorf("distillTokens(400) = %d, want more than the %d floor", got, distillMaxTokens)
	}
	if got := distillTokens(220); got != distillMaxTokens {
		t.Errorf("distillTokens(220) = %d, want the historical %d (default route unchanged)", got, distillMaxTokens)
	}
}

// TestDistillRouteFailsOpen: a route kloo cannot build is NOT a failed run. The
// pass is bookkeeping; its fallback is the run's own model, which is what the
// agent did for every version before the route existed.
func TestDistillRouteFailsOpen(t *testing.T) {
	own := llmtest.JSON(t, completion)
	base := &Loop{
		Client:   llm.New(own.URL+"/v1", "own-model"),
		Model:    "own-model",
		Endpoint: own.URL + "/v1",
		Budget:   &stubBudget{tripAt: 1000},
	}

	t.Run("no client factory", func(t *testing.T) {
		l := *base
		l.DistillModel = "brief-model" // configured, but the CLI wired no builder
		_, model := l.distillRoute()
		if model != "own-model" {
			t.Fatalf("model = %q, want the fallback own-model", model)
		}
	})

	t.Run("factory returns nil", func(t *testing.T) {
		l := *base
		l.DistillModel = "brief-model"
		l.NewDistillClient = func(string, string) llm.LLMClient { return nil }
		c, model := l.distillRoute()
		if model != "own-model" || c != l.Client {
			t.Fatalf("route = (%v, %q), want the run's own client and model", c != nil, model)
		}
	})

	t.Run("endpoint defaults to the run's", func(t *testing.T) {
		l := *base
		l.DistillModel = "second-model" // same server, second model: the Ollama case
		var gotEP string
		l.NewDistillClient = func(ep, model string) llm.LLMClient {
			gotEP = ep
			return llm.New(ep, model)
		}
		if _, model := l.distillRoute(); model != "second-model" {
			t.Fatalf("model = %q, want second-model", model)
		}
		if gotEP != base.Endpoint {
			t.Errorf("endpoint = %q, want the run's %q", gotEP, base.Endpoint)
		}
	})
}

// TestDistillOffDisablesThePass covers the switch the config layer sets from
// --no-distill / "distill": {"enabled": false}, without needing KLOO_DISTILL in
// the environment (which a TUI session cannot change mid-run).
func TestDistillOffDisablesThePass(t *testing.T) {
	own := llmtest.JSON(t, completion)
	l := &Loop{
		Client:     llm.New(own.URL+"/v1", "own-model"),
		Model:      "own-model",
		DistillOff: true,
	}
	if d := l.distiller(context.Background()); d != nil {
		t.Fatal("DistillOff must turn the pass off; Assemble then drops the oldest entries as before")
	}
}

// TestRoutedDistillerRunsThroughCompaction is the end-to-end arm: a realistic
// run's history, the real working memory, a real summary overflow — and the brief
// must come from the ROUTED endpoint rather than the run's own.
//
// The unit tests above pin distillRoute in isolation; this one pins that the route
// survives into Memory.Assemble, which is where the closure is actually consumed.
func TestRoutedDistillerRunsThroughCompaction(t *testing.T) {
	own := llmtest.JSON(t, completion)
	brief := llmtest.JSON(t, completion)
	l := &Loop{
		Client:          llm.New(own.URL+"/v1", "own-model"),
		Model:           "own-model",
		Endpoint:        own.URL + "/v1",
		Budget:          &stubBudget{tripAt: 100000},
		DistillModel:    "brief-model",
		DistillEndpoint: brief.URL + "/v1",
		NewDistillClient: func(ep, model string) llm.LLMClient {
			return llm.New(ep, model)
		},
	}
	mem := &workingMemory{}
	in := MemoryInput{
		Task:         "make the tabs Home/Apps/Profile",
		Convo:        realisticRun(240),
		WindowTokens: 131072,
		Distill:      l.distiller(context.Background()),
	}
	if _, err := mem.Assemble(in); err != nil {
		t.Fatalf("Assemble: %v", err)
	}
	if mem.distilled == 0 {
		t.Fatal("nothing was distilled, so this proves nothing about the route")
	}
	calls := brief.ModelCalls()
	if len(calls) == 0 {
		t.Fatalf("%d entries were distilled but the routed endpoint was never called", mem.distilled)
	}
	if n := len(own.ModelCalls()); n != 0 {
		t.Errorf("the run's own endpoint wrote %d brief(s); the route must take all of them", n)
	}
	for _, c := range calls {
		if !strings.Contains(string(c), `"brief-model"`) {
			t.Fatalf("a brief was written by the wrong model: %s", c)
		}
	}
	t.Logf("%d entries distilled into %d brief(s), all on the routed model", mem.distilled, len(calls))
}
