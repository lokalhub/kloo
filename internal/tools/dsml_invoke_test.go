package tools

import (
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
)

// TestExtractInvokeToolCalls covers the DeepSeek <｜DSML｜invoke …｜> dialect the
// native adapter recovers from text when the provider doesn't emit native
// tool_calls. The ｜ here is U+FF5C, exactly as the model emits it.
func TestExtractInvokeToolCalls(t *testing.T) {
	// The finish leak from the real deepseek-v4-flash run (prose before the call).
	content := `The build ran successfully with no errors. Would you like to see the full source?<｜DSML｜tool_calls>
<｜DSML｜invoke name="finish">
<｜DSML｜parameter name="summary" string="true">Built an Ionic Angular app with 3 tabs (Home, Apps, Profile).</｜DSML｜parameter>
</｜DSML｜invoke>
</｜DSML｜tool_calls>`

	calls := extractInvokeToolCalls(content)
	if len(calls) != 1 {
		t.Fatalf("want 1 call, got %d: %+v", len(calls), calls)
	}
	if calls[0].Name != "finish" {
		t.Errorf("name = %q, want finish", calls[0].Name)
	}
	if s, _ := calls[0].Args["summary"].(string); !strings.HasPrefix(s, "Built an Ionic Angular app") || strings.Contains(s, "DSML") {
		t.Errorf("summary = %q (should be the clean value, no DSML leak)", s)
	}
}

// TestExtractInvokeToolCalls_EditWithMarkupValue: a value containing HTML/template
// '<' must survive intact, and a run_command-style call must parse its command.
func TestExtractInvokeToolCalls_EditValue(t *testing.T) {
	content := `<｜DSML｜invoke name="write_file">
<｜DSML｜parameter name="path">src/app/home/home.page.html</｜DSML｜parameter>
<｜DSML｜parameter name="content"><ion-content><div class="center">Home</div></ion-content></｜DSML｜parameter>
</｜DSML｜invoke>`

	calls := extractInvokeToolCalls(content)
	if len(calls) != 1 || calls[0].Name != "write_file" {
		t.Fatalf("want 1 write_file call, got %+v", calls)
	}
	if got := calls[0].Args["path"]; got != "src/app/home/home.page.html" {
		t.Errorf("path = %q", got)
	}
	want := `<ion-content><div class="center">Home</div></ion-content>`
	if got, _ := calls[0].Args["content"].(string); got != want {
		t.Errorf("content = %q\nwant      %q", got, want)
	}
}

// TestExtractInvokeToolCalls_None: plain prose (or the already-handled dialects)
// yields no invoke calls, so the fallback chain is unaffected.
func TestExtractInvokeToolCalls_None(t *testing.T) {
	if got := extractInvokeToolCalls("Just a normal sentence, no tool call."); got != nil {
		t.Errorf("want nil, got %+v", got)
	}
}

// TestNativeAdapterRecoversInvokeFinish is the end-to-end guard: a native-FC reply
// with NO structured tool_calls but the DSML finish in content parses to one
// finish call through the adapter's text fallback.
func TestNativeAdapterRecoversInvokeFinish(t *testing.T) {
	msg := llm.Message{Role: llm.RoleAssistant, Content: "done.<｜DSML｜invoke name=\"finish\"><｜DSML｜parameter name=\"summary\">all set</｜DSML｜parameter></｜DSML｜invoke>"}
	call, err := NativeFCAdapter{}.Parse(msg)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if call.Name != "finish" || call.Args["summary"] != "all set" {
		t.Errorf("recovered call = %+v", call)
	}
}
