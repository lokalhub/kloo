package mcp

import (
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/tools"
)

func TestToParamSchema(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  tools.ParamSchema
	}{
		{
			name: "typical object schema",
			input: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":  map[string]any{"type": "string", "description": "file path"},
					"count": map[string]any{"type": "number", "description": "how many"},
					"flag":  map[string]any{"type": "boolean"},
				},
				"required": []any{"path"},
			},
			want: tools.ParamSchema{
				Properties: map[string]tools.Property{
					"path":  {Type: "string", Description: "file path"},
					"count": {Type: "number", Description: "how many"},
					"flag":  {Type: "boolean"},
				},
				Required: []string{"path"},
			},
		},
		{
			name: "nested object and array types preserved verbatim",
			input: map[string]any{
				"properties": map[string]any{
					"filter": map[string]any{"type": "object", "description": "a filter object"},
					"tags":   map[string]any{"type": "array", "description": "list of tags"},
				},
			},
			want: tools.ParamSchema{
				Properties: map[string]tools.Property{
					"filter": {Type: "object", Description: "a filter object"},
					"tags":   {Type: "array", Description: "list of tags"},
				},
			},
		},
		{
			name: "multiple required sorted",
			input: map[string]any{
				"properties": map[string]any{
					"a": map[string]any{"type": "string"},
					"b": map[string]any{"type": "string"},
				},
				"required": []any{"b", "a"},
			},
			want: tools.ParamSchema{
				Properties: map[string]tools.Property{
					"a": {Type: "string"},
					"b": {Type: "string"},
				},
				Required: []string{"a", "b"},
			},
		},
		{name: "nil input ⇒ empty", input: nil, want: tools.ParamSchema{}},
		{name: "non-object input ⇒ empty", input: "not a schema", want: tools.ParamSchema{}},
		{
			name:  "object without properties ⇒ empty",
			input: map[string]any{"type": "object"},
			want:  tools.ParamSchema{},
		},
		{
			name:  "properties present but required absent",
			input: map[string]any{"properties": map[string]any{"x": map[string]any{"type": "string"}}},
			want:  tools.ParamSchema{Properties: map[string]tools.Property{"x": {Type: "string"}}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toParamSchema(tc.input)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("toParamSchema()\n got: %+v\nwant: %+v", got, tc.want)
			}
		})
	}
}

var nameCharset = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

func TestToolName(t *testing.T) {
	cases := []struct {
		name, server, tool, want string
	}{
		{name: "normal join", server: "mempalace", tool: "recall", want: "mempalace__recall"},
		{name: "dots become underscores", server: "io.fs", tool: "read.file", want: "io_fs__read_file"},
		{name: "spaces become underscores", server: "my srv", tool: "do thing", want: "my_srv__do_thing"},
		{name: "hyphen preserved", server: "doc-server", tool: "list-rooms", want: "doc-server__list-rooms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := toolName(tc.server, tc.tool)
			if got != tc.want {
				t.Errorf("toolName(%q,%q) = %q, want %q", tc.server, tc.tool, got, tc.want)
			}
			if !nameCharset.MatchString(got) {
				t.Errorf("toolName(%q,%q) = %q violates %v", tc.server, tc.tool, got, nameCharset)
			}
		})
	}
}

func TestToolNameUnicodeSanitized(t *testing.T) {
	got := toolName("café", "naïve")
	if !nameCharset.MatchString(got) {
		t.Errorf("unicode tool name %q is not in the legal charset", got)
	}
	if strings.ContainsAny(got, "éï") {
		t.Errorf("unicode survived sanitization: %q", got)
	}
}

func TestToolNameDeterministic(t *testing.T) {
	a := toolName("server", "tool")
	b := toolName("server", "tool")
	if a != b {
		t.Errorf("toolName not deterministic: %q vs %q", a, b)
	}
}

func TestToolNameTruncation(t *testing.T) {
	long := strings.Repeat("x", 80)
	got := toolName(long, "tool")
	if len(got) > 64 {
		t.Errorf("toolName length = %d, want ≤ 64 (%q)", len(got), got)
	}
	if !nameCharset.MatchString(got) {
		t.Errorf("truncated name %q violates charset", got)
	}
}

// Two distinct tools whose names share the same 64-char prefix must still map to
// distinct kloo names via the deterministic numeric suffix.
func TestToolNameTruncationCollisionDistinct(t *testing.T) {
	prefix := strings.Repeat("a", 70)
	n1 := toolName(prefix, "alpha")
	n2 := toolName(prefix, "beta")
	if n1 == n2 {
		t.Errorf("distinct tools collided after truncation: both %q", n1)
	}
	if len(n1) > 64 || len(n2) > 64 {
		t.Errorf("truncated names exceed 64: %q (%d), %q (%d)", n1, len(n1), n2, len(n2))
	}
}

// A mapped schema must round-trip through ParamSchema.JSONSchema() to a valid
// object schema the native-FC adapter would accept.
func TestToParamSchemaRoundTrip(t *testing.T) {
	in := map[string]any{
		"properties": map[string]any{
			"q": map[string]any{"type": "string", "description": "query"},
		},
		"required": []any{"q"},
	}
	js := toParamSchema(in).JSONSchema()
	if js["type"] != "object" {
		t.Errorf("round-trip type = %v, want object", js["type"])
	}
	props, ok := js["properties"].(map[string]any)
	if !ok || props["q"] == nil {
		t.Errorf("round-trip properties missing q: %+v", js)
	}
	req, ok := js["required"].([]string)
	if !ok || len(req) != 1 || req[0] != "q" {
		t.Errorf("round-trip required = %v, want [q]", js["required"])
	}
}
