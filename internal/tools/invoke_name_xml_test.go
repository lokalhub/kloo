package tools

import (
	"errors"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
)

// observedReply is verbatim what deepseek-v4-flash emitted through OpenRouter on
// a real run. Before the fix it parsed to zero calls and the run stopped at step
// one reporting `answered`.
const observedReply = `<function>
<tool_call>
<invoke_name>run_command</invoke_name>
<parameters>
<command>go test ./...</command>
</parameters>
</tool_call>
</function>`

// TestExtractInvokeNameToolCalls pins recovery of the observed dialect.
func TestExtractInvokeNameToolCalls(t *testing.T) {
	calls := extractInvokeNameToolCalls(observedReply)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0].Name != "run_command" {
		t.Errorf("name = %q, want run_command", calls[0].Name)
	}
	if got := calls[0].Args["command"]; got != "go test ./..." {
		t.Errorf("command = %v, want %q", got, "go test ./...")
	}
}

// TestExtractInvokeNameWithoutWrapper: we key on the inner markers, so a reply
// that drops <function> or renames the wrapper still parses.
func TestExtractInvokeNameWithoutWrapper(t *testing.T) {
	calls := extractInvokeNameToolCalls(
		"<invoke_name>read_file</invoke_name><parameters><path>main.go</path></parameters>")
	if len(calls) != 1 || calls[0].Name != "read_file" || calls[0].Args["path"] != "main.go" {
		t.Fatalf("got %+v, want one read_file(path=main.go)", calls)
	}
}

// TestExtractInvokeNameBatched: two calls in one reply must not merge — the
// second call's markup swallowed into the first is how the <function=…> dialect
// used to corrupt edits.
func TestExtractInvokeNameBatched(t *testing.T) {
	calls := extractInvokeNameToolCalls(`
<invoke_name>read_file</invoke_name><parameters><path>a.go</path></parameters>
<invoke_name>read_file</invoke_name><parameters><path>b.go</path></parameters>`)
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0].Args["path"] != "a.go" || calls[1].Args["path"] != "b.go" {
		t.Errorf("paths = %v / %v, want a.go / b.go", calls[0].Args["path"], calls[1].Args["path"])
	}
}

// TestExtractInvokeNameValueContainingMarkup: an argument value is usually code
// and may contain '<'. Bounding on the matching close tag (not the next '<')
// keeps it intact.
func TestExtractInvokeNameValueContainingMarkup(t *testing.T) {
	calls := extractInvokeNameToolCalls(
		"<invoke_name>write_file</invoke_name><parameters><content>if a < b { x() }</content></parameters>")
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if got := calls[0].Args["content"]; got != "if a < b { x() }" {
		t.Errorf("content = %q, truncated at the '<'", got)
	}
}

// TestExtractInvokeNameIgnoresWrapperTags: </tool_call> and </function> must not
// become arguments.
func TestExtractInvokeNameIgnoresWrapperTags(t *testing.T) {
	calls := extractInvokeNameToolCalls(observedReply)
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if len(calls[0].Args) != 1 {
		t.Errorf("args = %v, want only the command parameter", calls[0].Args)
	}
}

// TestExtractInvokeNameNoFalsePositives: ordinary prose yields nothing.
func TestExtractInvokeNameNoFalsePositives(t *testing.T) {
	for _, s := range []string{
		"", "I'll run the tests next.",
		"<invoke_name>unterminated",
	} {
		if got := extractInvokeNameToolCalls(s); len(got) != 0 {
			t.Errorf("extract(%q) = %v, want none", s, got)
		}
	}
}

// TestUnparseableToolCallFailsLoudly is the more important half of the fix. An
// attempted-but-unparseable call must surface as an error so the loop issues a
// corrective re-prompt — NOT as zero calls, which the loop reads as "answered"
// and reports as a clean stop having done nothing.
func TestUnparseableToolCallFailsLoudly(t *testing.T) {
	a := NativeFCAdapter{}
	// A dialect no extractor knows, but obviously an attempted call.
	msg := llm.Message{Role: llm.RoleAssistant, Content: `<tool_call rpc="run_command" argv="go test" />`}

	_, err := a.ParseAll(msg)
	if err == nil {
		t.Fatal("an unparseable tool call must be an error, not a silent zero-call answer")
	}
	if !errors.Is(err, ErrMalformedToolCall) {
		t.Errorf("err = %v, want ErrMalformedToolCall so the loop re-prompts", err)
	}
}

// TestPlainProseStillAnswers: the loud failure must not fire on a genuine prose
// answer, or every conversational reply becomes an error.
func TestPlainProseStillAnswers(t *testing.T) {
	a := NativeFCAdapter{}
	msg := llm.Message{Role: llm.RoleAssistant, Content: "The tests pass. Nothing further to do."}

	calls, err := a.ParseAll(msg)
	if err != nil {
		t.Fatalf("prose reply should not error: %v", err)
	}
	if len(calls) != 0 {
		t.Errorf("prose reply produced %d calls", len(calls))
	}
}

// TestObservedDialectParsesThroughAdapter is the end-to-end guard: the exact
// reply that broke a real run now yields a usable call.
func TestObservedDialectParsesThroughAdapter(t *testing.T) {
	a := NativeFCAdapter{}
	calls, err := a.ParseAll(llm.Message{Role: llm.RoleAssistant, Content: observedReply})
	if err != nil {
		t.Fatalf("ParseAll: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "run_command" {
		t.Fatalf("got %+v, want one run_command call", calls)
	}
	if !strings.Contains(calls[0].Args["command"].(string), "go test") {
		t.Errorf("command = %v", calls[0].Args["command"])
	}
}

// TestNameTagVariants: the model switched from <invoke_name> to <tool_name>
// between one turn and its own corrective re-prompt, so the spelling is not
// stable even within a single run. Match the family.
func TestNameTagVariants(t *testing.T) {
	for _, tag := range nameTags {
		body := "<tool_call><" + tag + ">run_command</" + tag + "><parameters><command>go test</command></parameters></tool_call>"
		calls := extractInvokeNameToolCalls(body)
		if len(calls) != 1 {
			t.Errorf("<%s>: got %d calls, want 1", tag, len(calls))
			continue
		}
		if calls[0].Name != "run_command" || calls[0].Args["command"] != "go test" {
			t.Errorf("<%s>: got %+v", tag, calls[0])
		}
	}
}

// TestObservedSecondVariantThroughAdapter is the exact reply from the corrective
// re-prompt that still failed after the first fix.
func TestObservedSecondVariantThroughAdapter(t *testing.T) {
	reply := `<tool_call>
<tool_name>run_command</tool_name>
<parameters>
<command>go test ./...</command>
</parameters>
</tool_call>`
	calls, err := NativeFCAdapter{}.ParseAll(llm.Message{Role: llm.RoleAssistant, Content: reply})
	if err != nil {
		t.Fatalf("ParseAll: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "run_command" {
		t.Fatalf("got %+v, want one run_command call", calls)
	}
}
