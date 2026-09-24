package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// allSentMessages returns the role/content pairs of EVERY captured request, so a
// test can assert a property ACROSS turns rather than within one.
func allSentMessages(t *testing.T, srv *llmtest.Server) [][]struct {
	Role    string `json:"role"`
	Content string `json:"content"`
} {
	t.Helper()
	var out [][]struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	for _, raw := range srv.Requests() {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("request not JSON: %v", err)
		}
		out = append(out, body.Messages)
	}
	return out
}

const mapMarker = "Repository map"

// findMap returns the index of the message carrying the repo map, or -1.
func findMap(msgs []struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}) int {
	for i, m := range msgs {
		if strings.Contains(m.Content, mapMarker) {
			return i
		}
	}
	return -1
}

// TestMapPinnedIsFixedIndexAndFrozen is the caching property the default exists
// for. A prompt is served from a provider's prefix cache only up to the first
// token that differs, so the map must be BOTH at a fixed index and identical
// turn to turn. Either alone fails: a volatile block in front invalidates
// everything below it, and a stable block at the tail is displaced by every
// appended message.
//
// Measured before this was written (kloo-bench C07, muse-glimmer-30b, via a
// logging proxy): at the tail the map landed at index 1,3,5...47 with 41
// distinct bodies and 36.8% of the prompt reusable; pinned it sat at index 1
// with ONE body and 95.2% reusable, and the median call went 19.10s -> 2.66s.
func TestMapPinnedIsFixedIndexAndFrozen(t *testing.T) {
	root := genRepo(t, 14, 12)
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "file00.go"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "file01.go"}})},
		llmtest.Mock{Body: toolResp(t, 5, tcSpec{"read_file", map[string]any{"path": "file02.go"}})},
	)
	loop := memLoop(t, srv, root, 2000, NewWorkingMemory())
	loop.MapPosition = MapPositionPinned
	loop.System = "you are kloo"

	if _, err := loop.Run(context.Background(), "look at File3Func2DoesSomething"); err != nil {
		t.Fatalf("run: %v", err)
	}
	reqs := allSentMessages(t, srv)
	if len(reqs) < 2 {
		t.Fatalf("need at least 2 turns to test the across-turn property, got %d", len(reqs))
	}

	idx, body := -1, ""
	for n, msgs := range reqs {
		i := findMap(msgs)
		if i < 0 {
			t.Fatalf("turn %d: no repo map in the prompt", n)
		}
		if n == 0 {
			idx, body = i, msgs[i].Content
			// Adjacent user messages are merged by the request builder, so the
			// pinned map arrives concatenated onto the task at index 1 rather than
			// as its own message at 2. Either is fine; what matters is that it sits
			// ABOVE the history, so everything below it stays a cacheable prefix.
			if idx > 2 {
				t.Errorf("map at index %d, want <= 2 — a pinned map must sit above the history", idx)
			}
			continue
		}
		if i != idx {
			t.Errorf("turn %d: map moved to index %d (was %d) — a map that moves ends the cacheable prefix where it used to be", n, i, idx)
		}
		if msgs[i].Content != body {
			t.Errorf("turn %d: map content changed (%d bytes vs %d) — re-curating defeats pinning it", n, len(msgs[i].Content), len(body))
		}
	}
}

// TestMapDefaultIsPinned guards the shipped default. An unset Loop must not get
// a different layout from the CLI's, or a direct embedder silently runs the slow
// one.
func TestMapDefaultIsPinned(t *testing.T) {
	t.Setenv("KLOO_MAP_POSITION", "")
	l := &Loop{}
	if got := l.mapPosition(); got != MapPositionPinned {
		t.Errorf("unset MapPosition resolved to %q, want %q", got, MapPositionPinned)
	}
}

// TestMapPositionEnvOverrides keeps the benchmark seam working: the harness sets
// env, not flags, so an experiment can select an arm without a harness change.
func TestMapPositionEnvOverrides(t *testing.T) {
	t.Setenv("KLOO_MAP_POSITION", "tail")
	l := &Loop{MapPosition: MapPositionPinned}
	if got := l.mapPosition(); got != MapPositionTail {
		t.Errorf("env override gave %q, want %q", got, MapPositionTail)
	}
}
