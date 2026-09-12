package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// goldenRequestShape is the representative request whose v0.16.7 serialization is
// committed at testdata/request-v0.16.7.golden. Keep it in sync with the capture:
// the golden was produced by marshalling exactly this value on the baseline tree,
// BEFORE cache_control existed.
func goldenRequestShape() ChatRequest {
	return ChatRequest{
		Model: "test-model",
		Messages: []Message{
			{Role: RoleSystem, Content: "you are kloo"},
			{Role: RoleUser, Content: "make the failing check pass"},
			{Role: RoleAssistant, ToolCalls: []ToolCall{
				{ID: "c0", Type: "function", Function: FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}},
			}},
			{Role: RoleUser, Content: "tool read_file result:\npackage main\n"},
			{Role: RoleUser, Content: "Last verify: go test ./...\npassed=false exit=1\nFAIL"},
		},
		Temperature: 0.1,
		Tools: []Tool{{Type: "function", Function: ToolFunction{
			Name: "read_file", Description: "Read a file.",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
		}}},
	}
}

// TestRequestBodyUnchangedWhenCacheOff: with no message marked, the serialized
// body is BYTE-IDENTICAL to the golden captured from v0.16.7. This is the "never
// break other providers" guarantee — an endpoint that has never heard of
// cache_control must see exactly the request it saw before.
func TestRequestBodyUnchangedWhenCacheOff(t *testing.T) {
	want, err := os.ReadFile("testdata/request-v0.16.7.golden")
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	got, err := json.Marshal(goldenRequestShape())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("request body changed for an UNMARKED request.\n got: %s\nwant: %s", got, want)
	}
}

// TestMarkedMessageSerializesContentParts: exactly the marked message becomes a
// content-parts array carrying cache_control; every other message keeps a plain
// string content.
func TestMarkedMessageSerializesContentParts(t *testing.T) {
	req := goldenRequestShape()
	const marked = 3
	req.Messages[marked].CacheControl = CacheControlEphemeral()

	b, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	arrays := 0
	for i, m := range decoded.Messages {
		isArray := len(m.Content) > 0 && m.Content[0] == '['
		if i == marked {
			if !isArray {
				t.Fatalf("marked message[%d] must be a parts array, got %s", i, m.Content)
			}
			arrays++
			var parts []contentPart
			if err := json.Unmarshal(m.Content, &parts); err != nil {
				t.Fatalf("parts: %v", err)
			}
			if len(parts) != 1 || parts[0].Type != "text" {
				t.Fatalf("want one text part, got %+v", parts)
			}
			if parts[0].CacheControl == nil || parts[0].CacheControl.Type != "ephemeral" {
				t.Fatalf("cache_control missing or wrong: %+v", parts[0].CacheControl)
			}
			if parts[0].Text != req.Messages[marked].Content {
				t.Errorf("part text = %q, want %q", parts[0].Text, req.Messages[marked].Content)
			}
			continue
		}
		if isArray {
			t.Errorf("message[%d] must stay a plain string, got %s", i, m.Content)
		}
		if len(m.Content) > 0 {
			var s string
			if err := json.Unmarshal(m.Content, &s); err != nil {
				t.Errorf("message[%d] content is not a JSON string: %s", i, m.Content)
			}
		}
	}
	if arrays != 1 {
		t.Errorf("exactly one message may be a parts array, got %d", arrays)
	}
}

// cacheNoticeRe is the AGREED wording from the S8 mock
// (plan/artifacts/mocks/prompt-cache-notice.txt), matched in full rather than by a
// convenient substring.
var cacheNoticeRe = regexp.MustCompile(
	`^prompt cache rejected by endpoint \(.*\) — retried without it; prompt caching is off for the rest of this run$`)

// rejectingServer 400s any body containing cache_control and 200s otherwise,
// recording every body it saw.
func rejectingServer(t *testing.T) (*httptest.Server, *[][]byte) {
	t.Helper()
	var mu sync.Mutex
	var bodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := readAll(t, r)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		if strings.Contains(string(b), "cache_control") {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"unknown field \"cache_control\""}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}],"usage":{"total_tokens":5}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func markedRequest() ChatRequest {
	req := goldenRequestShape()
	req.Messages[3].CacheControl = CacheControlEphemeral()
	return req
}

// TestCacheControlRejectionFallsBack: an endpoint that refuses cache_control gets
// exactly one retry without the marker, the call succeeds, and the user is told
// once, in the agreed wording.
func TestCacheControlRejectionFallsBack(t *testing.T) {
	srv, bodies := rejectingServer(t)
	var notices []string
	c := New(srv.URL+"/v1", "test-model", WithLogf(func(format string, args ...any) {
		notices = append(notices, fmt.Sprintf(format, args...))
	}))

	resp, err := c.Complete(context.Background(), markedRequest())
	if err != nil {
		t.Fatalf("Complete must succeed via the fallback: %v", err)
	}
	if len(resp.Choices) == 0 || resp.Choices[0].Message.Content != "ok" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if len(*bodies) != 2 {
		t.Fatalf("server saw %d requests, want exactly 2 (one rejected, one retried)", len(*bodies))
	}
	if !strings.Contains(string((*bodies)[0]), "cache_control") {
		t.Error("request 1 must carry cache_control")
	}
	if strings.Contains(string((*bodies)[1]), "cache_control") {
		t.Error("request 2 must NOT carry cache_control")
	}
	if len(notices) != 1 {
		t.Fatalf("want exactly one notice, got %d: %q", len(notices), notices)
	}
	if !cacheNoticeRe.MatchString(notices[0]) {
		t.Errorf("notice does not match the agreed S8 wording:\n got: %s\nwant: %s", notices[0], cacheNoticeRe)
	}
}

// TestCacheControlRejectionRememberedForRun: after one rejection that Client stops
// sending the marker (no retry storm) — AND a second Client pointed at a fresh,
// accepting endpoint still sends it. The second half is what fails if the memory
// is a package-level var: one rejecting endpoint would silently disable caching
// process-wide.
func TestCacheControlRejectionRememberedForRun(t *testing.T) {
	srv, bodies := rejectingServer(t)
	c := New(srv.URL+"/v1", "test-model")

	for i := range 4 {
		if _, err := c.Complete(context.Background(), markedRequest()); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	// Call 1 costs two requests (reject + retry); calls 2-4 cost one each.
	if len(*bodies) != 5 {
		t.Fatalf("server saw %d requests, want 5 — a retry storm means the rejection was not remembered", len(*bodies))
	}
	marked := 0
	for _, b := range *bodies {
		if strings.Contains(string(b), "cache_control") {
			marked++
		}
	}
	if marked != 1 {
		t.Errorf("only the FIRST request may carry cache_control, got %d marked", marked)
	}

	// A different Client, a different endpoint: unaffected by the rejection above.
	var freshBodies [][]byte
	var mu sync.Mutex
	fresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := readAll(t, r)
		mu.Lock()
		freshBodies = append(freshBodies, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(fresh.Close)

	if _, err := New(fresh.URL+"/v1", "test-model").Complete(context.Background(), markedRequest()); err != nil {
		t.Fatalf("second client: %v", err)
	}
	if len(freshBodies) != 1 || !strings.Contains(string(freshBodies[0]), "cache_control") {
		t.Errorf("a SECOND client on an accepting endpoint must still send the marker — "+
			"the rejection memory must be per-Client, not package-level. bodies=%d", len(freshBodies))
	}
}

// TestNonRetryable4xxStillNotRetried: the fallback must not widen the retry
// ladder. A 400 that says nothing about cache_control is returned as an error
// after exactly one request.
func TestNonRetryable4xxStillNotRetried(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readAll(t, r)
		mu.Lock()
		hits++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"context length exceeded"}}`)
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL+"/v1", "test-model")
	if _, err := c.Complete(context.Background(), markedRequest()); err == nil {
		t.Fatal("an unrelated 400 must return an error, not be swallowed by the fallback")
	}
	if hits != 1 {
		t.Errorf("endpoint was hit %d times, want exactly 1 — the fallback must not retry an unrelated 4xx", hits)
	}
}

func readAll(t *testing.T, r *http.Request) []byte {
	t.Helper()
	b := make([]byte, 0, 4096)
	buf := make([]byte, 4096)
	for {
		n, err := r.Body.Read(buf)
		b = append(b, buf[:n]...)
		if err != nil {
			break
		}
	}
	return b
}

// The merge guard was ONE-SIDED: it checked whether the PREVIOUS message carried a
// breakpoint, but an INCOMING one was merged in and its marker copied nowhere. The
// request then went out with no breakpoint at all — no error, no rejection, just a
// silently worse cache hit rate that nothing attributes to anything.
//
// Found by the lead lens reviewing Phase 01 of the J1 rails/caching initiative.
func TestCacheBreakpointSurvivesSameRoleMerge(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Content: "first"},
		{Role: RoleUser, Content: "second", CacheControl: CacheControlEphemeral()},
	}
	out := normalizeMessages(in)

	if len(out) != 1 {
		t.Fatalf("want the two same-role messages merged into 1, got %d", len(out))
	}
	if out[0].CacheControl == nil {
		t.Fatal("the incoming breakpoint was swallowed by the merge; the request would " +
			"carry no cache_control at all and degrade silently")
	}
	if !strings.Contains(out[0].Content, "first") || !strings.Contains(out[0].Content, "second") {
		t.Errorf("merge lost content: %q", out[0].Content)
	}
}

// The pre-existing guard must still hold: a breakpoint on the PREVIOUS message
// blocks the merge entirely, because merging would move that marker BELOW content
// it was placed above and cache something volatile.
func TestBreakpointOnPreviousMessageStillBlocksMerge(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Content: "cached prefix", CacheControl: CacheControlEphemeral()},
		{Role: RoleUser, Content: "volatile tail"},
	}
	out := normalizeMessages(in)

	if len(out) != 2 {
		t.Fatalf("want the merge blocked (2 messages), got %d — the breakpoint moved below "+
			"content it was placed above", len(out))
	}
	if out[0].CacheControl == nil {
		t.Error("the prefix breakpoint was lost")
	}
}

// Three in a row: the marker on the last must end up on the single merged message,
// not be dropped by the second merge after surviving the first.
func TestBreakpointSurvivesRepeatedMerges(t *testing.T) {
	in := []Message{
		{Role: RoleUser, Content: "a"},
		{Role: RoleUser, Content: "b"},
		{Role: RoleUser, Content: "c", CacheControl: CacheControlEphemeral()},
	}
	out := normalizeMessages(in)

	if len(out) != 1 {
		t.Fatalf("want 1 merged message, got %d", len(out))
	}
	if out[0].CacheControl == nil {
		t.Fatal("breakpoint lost across repeated merges")
	}
}
