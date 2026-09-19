package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm/llmtest"
)

// TestNoRepoMapIsOffByDefault: the repo map is core to how kloo assembles context,
// so dropping it is a structural change that stays opt-in.
func TestNoRepoMapIsOffByDefault(t *testing.T) {
	t.Setenv("KLOO_NO_MAP", "")
	if noRepoMap() {
		t.Error("the map is dropped with the flag unset")
	}
	for _, v := range []string{"1", "true", "yes", "on", "ON"} {
		t.Setenv("KLOO_NO_MAP", v)
		if !noRepoMap() {
			t.Errorf("KLOO_NO_MAP=%q did not switch the map off", v)
		}
	}
	t.Setenv("KLOO_NO_MAP", "0")
	if noRepoMap() {
		t.Error(`KLOO_NO_MAP="0" switched the map off`)
	}
}

// TestNoMapKeepsTheMapOffTheWire asserts the thing that matters: with the flag on,
// no repo-map text reaches the model at all. A flag that merely shrank the map
// would not test the hypothesis, which is that ENUMERATING the repo is what invites
// reading instead of editing — grok has no map and its glimmer edits early.
func TestNoMapKeepsTheMapOffTheWire(t *testing.T) {
	marker := repoMapSection("src/aaa_unique_marker.go\n")
	if strings.TrimSpace(marker) == "" {
		t.Skip("repoMapSection produced nothing; nothing to assert against")
	}
	probe := func(off bool) string {
		t.Setenv("KLOO_NO_MAP", map[bool]string{true: "1", false: ""}[off])
		srv := llmtest.Sequence(t, readSpin(t, 2)...)
		loop, _ := newLoop(t, srv, nil, &stubBudget{tripAt: 2}, &stubChurn{})
		loop.Memory = NewWorkingMemory()
		loop.ContextTokens = 32000
		_, _ = loop.Run(context.Background(), "fix the widget")
		var b strings.Builder
		for _, r := range srv.Requests() {
			b.Write(r)
		}
		return b.String()
	}
	on := probe(true)
	if strings.Contains(on, "repo map") || strings.Contains(on, "Repo map") {
		t.Error("KLOO_NO_MAP=1 still sent a repo-map section to the model")
	}
	_ = probe(false) // exercises the default path; its content depends on the workspace
}
