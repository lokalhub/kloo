package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lokalhub/kloo/internal/config"
	"github.com/lokalhub/kloo/internal/llm"
	"github.com/lokalhub/kloo/internal/llm/llmtest"
	"github.com/lokalhub/kloo/internal/repomap"
	"github.com/lokalhub/kloo/internal/tools"
)

// Phase P00 DoD (overview §9): a long multi-file run that would overflow a small
// pinned context window completes with the per-step prompt bounded, while the
// no-memory control's per-step prompt climbs past the window. Both run offline
// (mocked LLM, REAL loop + memory) — this is the merge bar (D1 + D2). The live
// snappy/llama.cpp confirmation (D3) is operator-run via benchmark/memory/run.sh.

const dodWindow = 8192 // the pinned small window the run must never exceed (overview §9)

// bigRepo writes a workspace with a red answer.txt plus nFiles bulky files (each
// ~bytesEach bytes) the model will "read", so each read observation is large
// enough that an unmanaged transcript overflows dodWindow within a few turns.
func bigRepo(t *testing.T, nFiles, bytesEach int) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "answer.txt"), []byte("wrong\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("the quick brown fox jumps over the lazy dog. ", bytesEach/45+1)
	for i := 0; i < nFiles; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("file%d.txt", i)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return canon
}

// dodScript drives a long multi-file run: read each big file in turn (the
// transcript balloons), then write the fix so the red verify goes green.
func dodScript(t *testing.T, nFiles int) []llmtest.Mock {
	t.Helper()
	mocks := make([]llmtest.Mock, 0, nFiles+1)
	for i := 0; i < nFiles; i++ {
		mocks = append(mocks, llmtest.Mock{Body: toolResp(t, 30, tcSpec{"read_file", map[string]any{"path": fmt.Sprintf("file%d.txt", i)}})})
	}
	args, _ := json.Marshal(map[string]any{"path": "answer.txt", "content": "right\n"})
	mocks = append(mocks, llmtest.Mock{Body: func() string {
		resp := llm.ChatResponse{
			Choices: []llm.Choice{{Message: llm.Message{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{
				{ID: "w", Type: "function", Function: llm.FunctionCall{Name: "write_file", Arguments: string(args)}}}}}},
			Usage: llm.Usage{TotalTokens: 30},
		}
		b, _ := json.Marshal(resp)
		return string(b)
	}()})
	return mocks
}

func dodLoop(t *testing.T, root string, srv *llmtest.Server, mem WorkingMemory) *Loop {
	t.Helper()
	ws, err := tools.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	return &Loop{
		Client:             llm.New(srv.URL+"/v1", "test-model"),
		Adapter:            tools.NativeFCAdapter{},
		Registry:           tools.DefaultRegistry(ws),
		Verifier:           NewCommandVerifier(ws, "grep -qx right answer.txt"),
		Budget:             NewBudget(config.Config{MaxSteps: 100, MaxTokens: 5_000_000}, time.Now),
		Churn:              NewChurnDetector(1000),         // high: the repeated red verify must not halt this long read-heavy run
		ExploreNudgeRounds: 1000, ExploreAbortRounds: 1000, // this DoD stress test reads MANY files on purpose; don't trip the exploration rail
		Root:          root,
		ContextTokens: dodWindow,
		System:        "you are kloo, fix the failing check",
		Memory:        mem,
	}
}

// maxPromptTokens returns the largest per-step prompt size (Σ ApproxTokens over
// each request's message contents) — the §9 per-step curve, read straight off
// the captured requests.
func maxPromptTokens(reqs [][]byte) int {
	maxT := 0
	for _, raw := range reqs {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			continue
		}
		sum := 0
		for _, m := range body.Messages {
			sum += repomap.ApproxTokens(m.Content)
		}
		if sum > maxT {
			maxT = sum
		}
	}
	return maxT
}

// D1 (merge-blocking, offline): memory ON ⇒ the long run COMPLETES (terminal
// success, not overflow-driven budget-exceeded) and EVERY step's prompt ≤ window.
func TestMemoryDoDBoundedAndCompletes(t *testing.T) {
	const nFiles = 14
	root := bigRepo(t, nFiles, 3200)
	srv := llmtest.Sequence(t, dodScript(t, nFiles)...)
	loop := dodLoop(t, root, srv, NewWorkingMemory())

	rep, err := loop.Run(context.Background(), "make the failing check pass by reading the files first")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Reason != ReasonSuccess {
		t.Fatalf("run did not complete cleanly: reason=%q (%s)", rep.Reason, rep.String())
	}
	reqs := srv.Requests()
	if len(reqs) < nFiles {
		t.Fatalf("expected a long multi-turn run, got %d requests", len(reqs))
	}
	for i, raw := range reqs {
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("request %d not JSON: %v", i, err)
		}
		sum := 0
		for _, m := range body.Messages {
			sum += repomap.ApproxTokens(m.Content)
		}
		if sum > dodWindow {
			t.Errorf("step %d prompt = %d tokens, exceeds the %d window (memory must bound every step)", i+1, sum, dodWindow)
		}
	}
	if rep.Compactions == 0 {
		t.Errorf("a run that overflows an 8k window should have compacted at least once")
	}
}

// D2 (merge-blocking, offline): the no-memory control of the SAME run drives the
// per-step prompt PAST the window — the baseline failure the fix removes.
func TestMemoryDoDControlOverflows(t *testing.T) {
	const nFiles = 14
	root := bigRepo(t, nFiles, 3200)
	srv := llmtest.Sequence(t, dodScript(t, nFiles)...)
	loop := dodLoop(t, root, srv, nil) // Memory == nil ⇒ the legacy "today" baseline

	if _, err := loop.Run(context.Background(), "make the failing check pass by reading the files first"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if peak := maxPromptTokens(srv.Requests()); peak <= dodWindow {
		t.Fatalf("control peak per-step prompt = %d, expected it to climb PAST the %d window (baseline failure not demonstrated)", peak, dodWindow)
	}
}
