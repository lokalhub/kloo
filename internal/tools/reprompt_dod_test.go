package tools

import (
	"context"
	"errors"
	"testing"

	"github.com/lokal/kloo/internal/llm/llmtest"
)

// TestDoDMalformedThenRepromptThenDispatch is the Phase-02 DoD "one retry then
// surface" proof at the dispatch level: a malformed first reply triggers exactly
// one corrective re-prompt; the good second reply parses and dispatches to the
// handler.
func TestDoDMalformedThenRepromptThenDispatch(t *testing.T) {
	ctx := context.Background()
	good := nativeToolCallBody(t, "edit_file", map[string]any{"path": "a.go", "diff": "```\n<<<<<<< SEARCH\nx\n=======\ny\n>>>>>>> REPLACE\n```"})
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: nativeMalformed}, // first: no tool call
		llmtest.Mock{Body: good},            // second: valid
	)

	call, err := ParseWithRetry(ctx, clientFor(srv), NativeFCAdapter{}, baseReq())
	if err != nil {
		t.Fatalf("ParseWithRetry: %v", err)
	}
	if n := len(srv.Requests()); n != 2 {
		t.Fatalf("request count = %d, want exactly 2 (one re-prompt)", n)
	}

	reg, got := spyRegistry()
	if _, err := reg.Dispatch(ctx, call); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if got.Name != "edit_file" || got.Args["path"] != "a.go" {
		t.Errorf("handler call = %#v", *got)
	}
}

// TestDoDMalformedTwiceSurfacesNoThirdRequest is the anti-spiral DoD: two
// malformed replies surface ErrToolCallUnrecoverable with NO third request.
func TestDoDMalformedTwiceSurfacesNoThirdRequest(t *testing.T) {
	srv := llmtest.Sequence(t,
		llmtest.Mock{Body: nativeMalformed},
		llmtest.Mock{Body: nativeMalformed},
		llmtest.Mock{Body: nativeGoodReadFile}, // must never be reached
	)
	_, err := ParseWithRetry(context.Background(), clientFor(srv), NativeFCAdapter{}, baseReq())
	if !errors.Is(err, ErrToolCallUnrecoverable) {
		t.Fatalf("want ErrToolCallUnrecoverable, got %v", err)
	}
	if n := len(srv.Requests()); n != 2 {
		t.Errorf("request count = %d, want exactly 2 (no third attempt)", n)
	}
}
