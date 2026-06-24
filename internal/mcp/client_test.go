package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/lokalhub/kloo/internal/config"
)

// newEchoAddServer builds an in-process MCP server exposing echo + add, reusing
// the handlers defined in connectivity_test.go (same package test scope).
func newEchoAddServer(opts *sdk.ServerOptions) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "test-srv", Version: "test"}, opts)
	sdk.AddTool(srv, &sdk.Tool{Name: "echo", Description: "echo text"}, echoHandler)
	sdk.AddTool(srv, &sdk.Tool{Name: "add", Description: "add a and b"}, addHandler)
	return srv
}

func toolNames(tools []*sdk.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	return names
}

func envContains(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// TestConfigFromEntries: the config.MCPServerEntry → mcp.ServerConfig adapter maps
// fields, casts ExposeMode, sorts servers by name (deterministic), and carries the cap.
func TestConfigFromEntries(t *testing.T) {
	entries := map[string]config.MCPServerEntry{
		"zeta": {URL: "http://z:1/mcp", ExposeMode: "lazy"},
		"alpha": {
			Command: "alpha-srv", Args: []string{"-x"}, Env: map[string]string{"K": "V"},
			Headers:    map[string]string{"Authorization": "Bearer test-token"},
			ExposeMode: "curated", Expose: []string{"a", "b"}, TimeoutSeconds: 12, Disabled: true,
		},
	}
	got := ConfigFromEntries(entries, 7)

	if got.MaxExposedTools != 7 {
		t.Errorf("MaxExposedTools = %d, want 7", got.MaxExposedTools)
	}
	if len(got.Servers) != 2 || got.Servers[0].Name != "alpha" || got.Servers[1].Name != "zeta" {
		t.Fatalf("servers not sorted by name: %+v", got.Servers)
	}
	wantAlpha := ServerConfig{
		Name: "alpha", Command: "alpha-srv", Args: []string{"-x"},
		Env: map[string]string{"K": "V"}, Headers: map[string]string{"Authorization": "Bearer test-token"}, ExposeMode: ExposeCurated,
		Expose: []string{"a", "b"}, TimeoutSec: 12, Disabled: true,
	}
	if !reflect.DeepEqual(got.Servers[0], wantAlpha) {
		t.Errorf("alpha\n got: %+v\nwant: %+v", got.Servers[0], wantAlpha)
	}
	if got.Servers[1].URL != "http://z:1/mcp" || got.Servers[1].ExposeMode != ExposeLazy {
		t.Errorf("zeta = %+v", got.Servers[1])
	}
}

// TestClientInMemory: connect to an in-process server over the in-memory
// transport, snapshot its tools, and Close — no node, no network. (Servers must
// connect before clients per the SDK.)
func TestClientInMemory(t *testing.T) {
	ctx := context.Background()
	srv := newEchoAddServer(nil)

	clientT, serverT := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()

	cl := sdk.NewClient(&sdk.Implementation{Name: clientImplName, Version: clientVersion}, nil)
	session, err := cl.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	tools, err := listAllTools(ctx, session)
	if err != nil {
		t.Fatalf("listAllTools: %v", err)
	}
	c := &Client{Name: "mem", session: session, tools: tools, timeout: DefaultCallTimeout}

	names := toolNames(c.Tools())
	if !contains(names, "echo") || !contains(names, "add") {
		t.Fatalf("ListTools names = %v, want echo+add", names)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestDialHTTPBehavioral: dial a real httptest.Server fronting the SDK's
// StreamableHTTPHandler over StreamableClientTransport, then ListTools + CallTool
// over HTTP — exercising the request/response path end-to-end on loopback (no
// external network). This is the QA-required HTTP behavioral coverage.
func TestDialHTTPBehavioral(t *testing.T) {
	ctx := context.Background()
	srv := newEchoAddServer(nil)
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
	ts := httptest.NewServer(handler)
	defer ts.Close()

	c, err := dial(ctx, ServerConfig{Name: "docs", URL: ts.URL}, DefaultConnectTimeout)
	if err != nil {
		t.Fatalf("dial HTTP: %v", err)
	}
	defer c.Close()

	if names := toolNames(c.Tools()); !contains(names, "echo") {
		t.Fatalf("HTTP ListTools = %v, want echo", names)
	}

	// CallTool over HTTP must round-trip a text result.
	res, err := c.session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"text": "hi-over-http"},
	})
	if err != nil {
		t.Fatalf("CallTool over HTTP: %v", err)
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("content[0] = %T, want *TextContent", res.Content[0])
	}
	if tc.Text != "hi-over-http" {
		t.Errorf("CallTool text = %q, want %q", tc.Text, "hi-over-http")
	}
}

func TestDialHTTPWithHeadersBehavioral(t *testing.T) {
	ctx := context.Background()
	srv := newEchoAddServer(nil)
	handler := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
	seen := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization header = %q, want bearer token", got)
		}
		if got := r.Header.Get("X-API-Key"); got != "test-api-key" {
			t.Errorf("X-API-Key header = %q, want test-api-key", got)
		}
		seen++
		handler.ServeHTTP(w, r)
	}))
	defer ts.Close()

	c, err := dial(ctx, ServerConfig{
		Name: "docs",
		URL:  ts.URL,
		Headers: map[string]string{
			"Authorization": "Bearer test-token",
			"X-API-Key":     "test-api-key",
		},
	}, DefaultConnectTimeout)
	if err != nil {
		t.Fatalf("dial HTTP with headers: %v", err)
	}
	defer c.Close()

	if names := toolNames(c.Tools()); !contains(names, "echo") {
		t.Fatalf("HTTP ListTools = %v, want echo", names)
	}

	res, err := c.session.CallTool(ctx, &sdk.CallToolParams{
		Name:      "echo",
		Arguments: map[string]any{"text": "hi-auth"},
	})
	if err != nil {
		t.Fatalf("CallTool over authenticated HTTP: %v", err)
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("content[0] = %T, want *TextContent", res.Content[0])
	}
	if tc.Text != "hi-auth" {
		t.Errorf("CallTool text = %q, want %q", tc.Text, "hi-auth")
	}
	if seen < 2 {
		t.Fatalf("expected headers on ListTools and CallTool HTTP requests, saw %d requests", seen)
	}
}

// TestTransport: the transport() constructor picks the right SDK transport and
// rejects ambiguous config.
func TestTransport(t *testing.T) {
	// stdio: argv + merged env.
	tr, err := ServerConfig{
		Name: "s", Command: "mycmd", Args: []string{"-a", "-b"},
		Env: map[string]string{"K": "V"},
	}.transport()
	if err != nil {
		t.Fatalf("stdio transport: %v", err)
	}
	ct, ok := tr.(*sdk.CommandTransport)
	if !ok {
		t.Fatalf("want *CommandTransport, got %T", tr)
	}
	if got := ct.Command.Args; len(got) != 3 || got[0] != "mycmd" || got[1] != "-a" || got[2] != "-b" {
		t.Errorf("argv = %v, want [mycmd -a -b]", got)
	}
	if !envContains(ct.Command.Env, "K=V") {
		t.Errorf("env missing K=V (got tail %v)", ct.Command.Env[len(ct.Command.Env)-1:])
	}

	// HTTP: endpoint set.
	tr, err = ServerConfig{Name: "h", URL: "http://x:1/mcp"}.transport()
	if err != nil {
		t.Fatalf("http transport: %v", err)
	}
	st, ok := tr.(*sdk.StreamableClientTransport)
	if !ok || st.Endpoint != "http://x:1/mcp" {
		t.Fatalf("want StreamableClientTransport{Endpoint:...}, got %T %+v", tr, tr)
	}
	if st.HTTPClient != http.DefaultClient {
		t.Fatalf("HTTP without headers should use http.DefaultClient, got %#v", st.HTTPClient)
	}

	// HTTP with headers: endpoint set, custom client/transport installed.
	tr, err = ServerConfig{Name: "h", URL: "http://x:1/mcp", Headers: map[string]string{"Authorization": "Bearer test-token"}}.transport()
	if err != nil {
		t.Fatalf("http transport with headers: %v", err)
	}
	st, ok = tr.(*sdk.StreamableClientTransport)
	if !ok || st.Endpoint != "http://x:1/mcp" {
		t.Fatalf("want StreamableClientTransport{Endpoint:...}, got %T %+v", tr, tr)
	}
	if st.HTTPClient == nil || st.HTTPClient == http.DefaultClient {
		t.Fatalf("HTTP with headers should use a custom client, got %#v", st.HTTPClient)
	}
	if _, ok := st.HTTPClient.Transport.(headerRoundTripper); !ok {
		t.Fatalf("custom client transport = %T, want headerRoundTripper", st.HTTPClient.Transport)
	}

	// stdio with headers / both / neither ⇒ ErrBadServerConfig.
	if _, err := (ServerConfig{Name: "stdio-headers", Command: "c", Headers: map[string]string{"Authorization": "Bearer test-token"}}).transport(); !errors.Is(err, ErrBadServerConfig) {
		t.Errorf("stdio with headers: want ErrBadServerConfig, got %v", err)
	}
	if _, err := (ServerConfig{Name: "both", Command: "c", URL: "u"}).transport(); !errors.Is(err, ErrBadServerConfig) {
		t.Errorf("both set: want ErrBadServerConfig, got %v", err)
	}
	if _, err := (ServerConfig{Name: "neither"}).transport(); !errors.Is(err, ErrBadServerConfig) {
		t.Errorf("neither set: want ErrBadServerConfig, got %v", err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestHeaderRoundTripper(t *testing.T) {
	original, err := http.NewRequest(http.MethodGet, "http://example.test/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	original.Header.Set("Authorization", "caller-owned")

	var gotReq *http.Request
	rt := headerRoundTripper{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotReq = req
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       http.NoBody,
				Request:    req,
			}, nil
		}),
		endpointScheme: "http",
		endpointHost:   "example.test",
		headers:        map[string]string{"Authorization": "Bearer test-token", "X-API-Key": "test-api-key"},
	}

	if _, err := rt.RoundTrip(original); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if gotReq == original {
		t.Fatal("same-origin request was mutated in place, want cloned request")
	}
	if got := gotReq.Header.Get("Authorization"); got != "Bearer test-token" {
		t.Errorf("forwarded Authorization = %q", got)
	}
	if got := gotReq.Header.Get("X-API-Key"); got != "test-api-key" {
		t.Errorf("forwarded X-API-Key = %q", got)
	}
	if got := original.Header.Get("Authorization"); got != "caller-owned" {
		t.Errorf("original Authorization mutated to %q", got)
	}

	crossOrigin, err := http.NewRequest(http.MethodGet, "http://other.example.test/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	gotReq = nil
	if _, err := rt.RoundTrip(crossOrigin); err != nil {
		t.Fatalf("cross-origin RoundTrip: %v", err)
	}
	if gotReq != crossOrigin {
		t.Fatal("cross-origin request should be delegated without cloning")
	}
	if got := gotReq.Header.Get("Authorization"); got != "" {
		t.Errorf("cross-origin Authorization = %q, want absent", got)
	}
}

func TestHeaderRoundTripperRedirectDoesNotLeakCrossOrigin(t *testing.T) {
	var crossOriginAuth, crossOriginAPIKey string
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		crossOriginAuth = r.Header.Get("Authorization")
		crossOriginAPIKey = r.Header.Get("X-API-Key")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	var sameOriginAuth string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sameOriginAuth = r.Header.Get("Authorization")
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	endpoint, err := http.NewRequest(http.MethodGet, origin.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: headerRoundTripper{
		endpointScheme: endpoint.URL.Scheme,
		endpointHost:   endpoint.URL.Host,
		headers: map[string]string{
			"Authorization": "Bearer test-token",
			"X-API-Key":     "test-api-key",
		},
	}}

	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatalf("redirect GET: %v", err)
	}
	_ = resp.Body.Close()
	if sameOriginAuth != "Bearer test-token" {
		t.Errorf("same-origin Authorization = %q, want bearer token", sameOriginAuth)
	}
	if crossOriginAuth != "" {
		t.Errorf("cross-origin Authorization = %q, want absent", crossOriginAuth)
	}
	if crossOriginAPIKey != "" {
		t.Errorf("cross-origin X-API-Key = %q, want absent", crossOriginAPIKey)
	}
}

// TestDialConnectTimeout: a server that accepts the connection but never responds
// to the MCP initialize handshake ⇒ dial returns an error bounded by the connect
// timeout, not a hang. An HTTP black-hole (handler blocks until the client
// cancels) is used so the bound is the connect deadline itself, with no stdio
// child-process SIGTERM grace to inflate the wall-time.
func TestDialConnectTimeout(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not on PATH")
	}
	// `sleep 30` starts but never speaks MCP, so the initialize handshake never
	// completes. Without the connect timeout, dial would block until the process
	// exits (~30s). With it, Connect aborts at the 200ms deadline and tears the
	// child down (the SDK waits a fixed ~5s grace before SIGTERM), so dial returns
	// in ~5s — proving startup is bounded, well under the 30s the server would
	// otherwise take. A watchdog catches a true hang rather than timing out the suite.
	type result struct {
		err     error
		elapsed time.Duration
	}
	res := make(chan result, 1)
	start := time.Now()
	go func() {
		_, err := dial(context.Background(),
			ServerConfig{Name: "hang", Command: "sleep", Args: []string{"30"}},
			200*time.Millisecond)
		res <- result{err, time.Since(start)}
	}()

	select {
	case r := <-res:
		if r.err == nil {
			t.Fatal("want a connect-timeout error, got nil")
		}
		if r.elapsed > 15*time.Second {
			t.Errorf("dial took %v; expected ~5s (200ms deadline + teardown grace), far below the 30s server", r.elapsed)
		}
	case <-time.After(25 * time.Second):
		t.Fatal("dial did not return: connect timeout not honored (hung past 25s)")
	}
}

// TestDialPaginationAccumulates: listAllTools follows NextCursor across pages. A
// server with PageSize 1 and 3 tools must yield all 3.
func TestDialPaginationAccumulates(t *testing.T) {
	ctx := context.Background()
	srv := sdk.NewServer(&sdk.Implementation{Name: "p", Version: "t"}, &sdk.ServerOptions{PageSize: 1})
	sdk.AddTool(srv, &sdk.Tool{Name: "echo"}, echoHandler)
	sdk.AddTool(srv, &sdk.Tool{Name: "add"}, addHandler)
	sdk.AddTool(srv, &sdk.Tool{Name: "third"}, echoHandler)

	clientT, serverT := sdk.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, serverT, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer ss.Close()

	cl := sdk.NewClient(&sdk.Implementation{Name: clientImplName, Version: clientVersion}, nil)
	session, err := cl.Connect(ctx, clientT, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer session.Close()

	tools, err := listAllTools(ctx, session)
	if err != nil {
		t.Fatalf("listAllTools: %v", err)
	}
	if len(tools) != 3 {
		t.Fatalf("paginated ListTools accumulated %d tools, want 3: %v", len(tools), toolNames(tools))
	}
}
