package tui

import "testing"

func TestAckReply(t *testing.T) {
	// Bare acknowledgments / closings → a canned reply, no run.
	acks := []string{
		"thanks", "Thanks!", "thank you", "thank you :)", "thanks 🙏",
		"ty", "THX", "much appreciated", "great", "Perfect!", "nice work",
		"ok", "okay", "got it", "sounds good", "lgtm", "that's all", "bye",
	}
	for _, s := range acks {
		if _, ok := ackReply(s); !ok {
			t.Errorf("ackReply(%q) = false, want a canned reply", s)
		}
	}

	// Real requests (even when they START with thanks) must NOT be swallowed.
	tasks := []string{
		"thanks, now add a settings tab",
		"thank you but the home tab is broken",
		"ok now build it",
		"add a profile page",
		"great, can you also center the title",
		"why did it fail?",
		"",
	}
	for _, s := range tasks {
		if reply, ok := ackReply(s); ok {
			t.Errorf("ackReply(%q) = %q, want it to pass through to a run", s, reply)
		}
	}
}
