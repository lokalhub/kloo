package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/agent"
	"github.com/lokalhub/kloo/internal/config"
)

// TestRestartCountersReachTheJSON: the summary builds its own counter struct rather
// than marshalling agent.ToolCounters, so a counter added to the agent is NOT
// automatically visible to anyone reading a run. The restart counters were added
// and the JSON kept omitting them — the first measured run came back
// "no-restart-counter", which makes an inert restart and a failed restart look
// identical, and that distinction is the only reason the counters exist.
func TestRestartCountersReachTheJSON(t *testing.T) {
	rep := &agent.Report{
		Reason: agent.ReasonSuccess,
		ToolCounters: agent.ToolCounters{
			Restarts:       1,
			RestartRescues: 1,
		},
	}
	s := buildRunSummary(config.Config{Model: "m"}, "go test ./...", rep, 0, nil)
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	for _, want := range []string{`"restarts":1`, `"restart_rescues":1`} {
		if !strings.Contains(got, want) {
			t.Errorf("run summary JSON is missing %s\n%s", want, got)
		}
	}
}

// TestRestartCountersAppearInTheCounterLine guards the human-readable line too: it
// is what shows up in a bench log tail, and it was equally blind.
func TestRestartCountersAppearInTheCounterLine(t *testing.T) {
	line := formatToolCounters(agent.ToolCounters{Restarts: 2, RestartRescues: 1})
	for _, want := range []string{"restarts=2", "restart_rescues=1"} {
		if !strings.Contains(line, want) {
			t.Errorf("counter line %q is missing %q", line, want)
		}
	}
}
