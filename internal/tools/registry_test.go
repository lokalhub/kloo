package tools

import (
	"context"
	"errors"
	"sort"
	"testing"
)

// spyTool records the Call it was dispatched, for routing assertions.
type spyTool struct {
	name   string
	got    *Call
	schema ParamSchema
}

func (s *spyTool) Name() string        { return s.name }
func (s *spyTool) Description() string { return "spy tool" }
func (s *spyTool) Schema() ParamSchema { return s.schema }
func (s *spyTool) Invoke(ctx context.Context, c Call) (Result, error) {
	*s.got = c
	return Result{Output: "ok"}, nil
}

func TestDefaultRegistryFiveTools(t *testing.T) {
	ws, _ := wsAt(t)
	reg := DefaultRegistry(ws)

	var got []string
	for _, tl := range reg.Tools() {
		got = append(got, tl.Name())
	}
	sort.Strings(got)
	want := []string{"edit_file", "list_dir", "read_file", "run_command", "write_file"}
	if len(got) != len(want) {
		t.Fatalf("got %d tools %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tool[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Spot-check schemas reach the registry with the right required args.
	checkRequired := func(name string, req ...string) {
		tl, ok := reg.Lookup(name)
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		gotReq := append([]string(nil), tl.Schema().Required...)
		sort.Strings(gotReq)
		sort.Strings(req)
		if len(gotReq) != len(req) {
			t.Errorf("%s required = %v, want %v", name, gotReq, req)
			return
		}
		for i := range req {
			if gotReq[i] != req[i] {
				t.Errorf("%s required = %v, want %v", name, gotReq, req)
			}
		}
	}
	checkRequired("read_file", "path")
	checkRequired("list_dir", "path")
	checkRequired("write_file", "path", "content")
	checkRequired("edit_file", "path", "diff")
	checkRequired("run_command", "command")
}

func TestDispatchRoutesToHandler(t *testing.T) {
	var got Call
	reg := NewRegistry()
	reg.Register(&spyTool{name: "read_file", got: &got, schema: ParamSchema{
		Properties: map[string]Property{"path": {Type: "string"}}, Required: []string{"path"},
	}})

	res, err := reg.Dispatch(context.Background(), Call{Name: "read_file", Args: map[string]any{"path": "x.go"}})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Output != "ok" {
		t.Errorf("result = %+v", res)
	}
	if got.Name != "read_file" || got.Args["path"] != "x.go" {
		t.Errorf("handler got wrong call: %+v", got)
	}
}

func TestDispatchUnknownTool(t *testing.T) {
	reg := NewRegistry()
	_, err := reg.Dispatch(context.Background(), Call{Name: "nope"})
	if !errors.Is(err, ErrUnknownTool) {
		t.Errorf("want ErrUnknownTool, got %v", err)
	}
}

func TestDispatchValidatesRequiredArgs(t *testing.T) {
	var got Call
	reg := NewRegistry()
	reg.Register(&spyTool{name: "read_file", got: &got, schema: ParamSchema{
		Properties: map[string]Property{"path": {Type: "string"}}, Required: []string{"path"},
	}})
	_, err := reg.Dispatch(context.Background(), Call{Name: "read_file", Args: map[string]any{}})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Errorf("want ErrInvalidArgs for missing path, got %v", err)
	}
}

func TestExactlyOneCall(t *testing.T) {
	if _, err := ExactlyOneCall(nil); !errors.Is(err, ErrNoToolCall) {
		t.Errorf("zero calls: want ErrNoToolCall, got %v", err)
	}
	two := []Call{{Name: "a"}, {Name: "b"}}
	if _, err := ExactlyOneCall(two); !errors.Is(err, ErrMultipleToolCalls) {
		t.Errorf("two calls: want ErrMultipleToolCalls, got %v", err)
	}
	one := []Call{{Name: "a", Args: map[string]any{"k": "v"}}}
	got, err := ExactlyOneCall(one)
	if err != nil || got.Name != "a" {
		t.Errorf("one call: got %+v err %v", got, err)
	}
}

func TestParamSchemaJSONSchema(t *testing.T) {
	s := ParamSchema{
		Properties: map[string]Property{"path": {Type: "string", Description: "a path"}},
		Required:   []string{"path"},
	}
	js := s.JSONSchema()
	if js["type"] != "object" {
		t.Errorf("type = %v", js["type"])
	}
	props, _ := js["properties"].(map[string]any)
	pathProp, _ := props["path"].(map[string]any)
	if pathProp["type"] != "string" {
		t.Errorf("path type = %v", pathProp["type"])
	}
	req, _ := js["required"].([]string)
	if len(req) != 1 || req[0] != "path" {
		t.Errorf("required = %v", req)
	}
}
