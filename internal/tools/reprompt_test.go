package tools

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/lokal/kloo/internal/llm"
	"github.com/lokal/kloo/internal/llm/llmtest"
)

// Canned native-FC responses for the scripted harness.
const (
	nativeGoodReadFile = `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`
	nativeMalformed    = `{"choices":[{"index":0,"message":{"role":"assistant","content":"I will just describe the plan instead of calling a tool."},"finish_reason":"stop"}]}`
)

func clientFor(srv *llmtest.Server) llm.LLMClient {
	return llm.New(srv.URL+"/v1", "snappy")
}

func baseReq() llm.ChatRequest {
	return llm.ChatRequest{Model: "snappy", Messages: []llm.Message{{Role: llm.RoleUser, Content: "read x"}}}
}

func TestParseWithRetryGoodFirst(t *testing.T) {
	srv := llmtest.Sequence(t, llmtest.Mock{Body: nativeGoodReadFile})
	call, err := ParseWithRetry(context.Background(), clientFor(srv), NativeFCAdapter{}, baseReq())
	if err != nil {
		t.Fatalf("ParseWithRetry: %v", err)
	}
	if call.Name != "read_file" || call.Args["path"] != "x" {
		t.Errorf("call = %+v", call)
	}
	if n := len(srv.Requests()); n != 1 {
		t.Errorf("request count = %d, want 1 (no re-prompt)", n)
	}
}

func TestParseWithRetryMalformedThenGood(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: nativeMalformed},
		llmtest.Mock{Body: nativeGoodReadFile},
	)
	call, err := ParseWithRetry(context.Background(), clientFor(srv), NativeFCAdapter{}, baseReq())
	if err != nil {
		t.Fatalf("ParseWithRetry: %v", err)
	}
	if call.Name != "read_file" {
		t.Errorf("call = %+v", call)
	}
	reqs := srv.Requests()
	if len(reqs) != 2 {
		t.Fatalf("request count = %d, want exactly 2 (one re-prompt)", len(reqs))
	}
	// The corrective re-prompt (2nd request) must restate the native contract.
	if !bytes.Contains(reqs[1], []byte("tool_call")) {
		t.Errorf("2nd request missing the native corrective text:\n%s", reqs[1])
	}
}

func TestParseWithRetryMalformedTwiceSurfaces(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: nativeMalformed},
		llmtest.Mock{Body: nativeMalformed},
		llmtest.Mock{Body: nativeGoodReadFile}, // must NOT be reached
	)
	_, err := ParseWithRetry(context.Background(), clientFor(srv), NativeFCAdapter{}, baseReq())
	if !errors.Is(err, ErrToolCallUnrecoverable) {
		t.Fatalf("want ErrToolCallUnrecoverable, got %v", err)
	}
	if n := len(srv.Requests()); n != 2 {
		t.Errorf("request count = %d, want exactly 2 (no third attempt)", n)
	}
}

func TestParseWithRetryXMLCorrectiveCitesGrammar(t *testing.T) {
	xmlMalformed := `{"choices":[{"index":0,"message":{"role":"assistant","content":"no tool here"}}]}`
	xmlGood := `{"choices":[{"index":0,"message":{"role":"assistant","content":"<tool name=\"read_file\"><arg name=\"path\">x</arg></tool>"}}]}`
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: xmlMalformed},
		llmtest.Mock{Body: xmlGood},
	)
	call, err := ParseWithRetry(context.Background(), clientFor(srv), XMLAdapter{}, baseReq())
	if err != nil {
		t.Fatalf("ParseWithRetry: %v", err)
	}
	if call.Name != "read_file" || call.Args["path"] != "x" {
		t.Errorf("call = %+v", call)
	}
	reqs := srv.Requests()
	if len(reqs) != 2 {
		t.Fatalf("request count = %d, want 2", len(reqs))
	}
	// The XML corrective must cite the grammar. JSON HTML-escapes "<" to
	// "<", so assert on stable phrases the corrective uses verbatim.
	if !bytes.Contains(reqs[1], []byte("XML format")) || !bytes.Contains(reqs[1], []byte("tool name=")) {
		t.Errorf("2nd request missing the XML grammar corrective:\n%s", reqs[1])
	}
}
