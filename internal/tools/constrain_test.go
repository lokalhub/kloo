package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
)

func TestApplyConstraintJSONSchema(t *testing.T) {
	ws, _ := wsAt(t)
	reg := DefaultRegistry(ws)
	req := ApplyConstraint(llm.ChatRequest{Model: "test-model"}, reg, EndpointCaps{SupportsJSONSchema: true})

	if req.ResponseFormat == nil {
		t.Fatal("expected a response_format to be set")
	}
	if req.Grammar != "" {
		t.Errorf("grammar should not be set when json_schema is used")
	}
	// The schema must be derived from the registry — every tool name appears.
	raw, _ := json.Marshal(req.ResponseFormat)
	for _, name := range []string{"read_file", "edit_file", "write_file", "list_dir", "run_command"} {
		if !strings.Contains(string(raw), name) {
			t.Errorf("json_schema missing tool %q (not derived from registry): %s", name, raw)
		}
	}
	if !strings.Contains(string(raw), "json_schema") {
		t.Errorf("response_format should be a json_schema: %s", raw)
	}
}

func TestApplyConstraintGrammar(t *testing.T) {
	ws, _ := wsAt(t)
	reg := DefaultRegistry(ws)
	req := ApplyConstraint(llm.ChatRequest{Model: "test-model"}, reg, EndpointCaps{SupportsGrammar: true})

	if req.Grammar == "" {
		t.Fatal("expected a GBNF grammar to be set")
	}
	if req.ResponseFormat != nil {
		t.Errorf("response_format should not be set when only grammar is supported")
	}
	// Grammar is derived from the registry tool names.
	if !strings.Contains(req.Grammar, "read_file") || !strings.Contains(req.Grammar, "run_command") {
		t.Errorf("grammar not derived from registry: %s", req.Grammar)
	}
}

// TestApplyConstraintNoOp is the key optional-behaviour assertion: when neither
// constraint is supported, the request is returned unchanged.
func TestApplyConstraintNoOp(t *testing.T) {
	ws, _ := wsAt(t)
	reg := DefaultRegistry(ws)
	base := llm.ChatRequest{Model: "test-model", Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}}}

	got := ApplyConstraint(base, reg, EndpointCaps{}) // no grammar, no json_schema

	if got.ResponseFormat != nil || got.Grammar != "" {
		t.Errorf("expected a graceful no-op, got response_format=%v grammar=%q", got.ResponseFormat, got.Grammar)
	}
	// Marshalling the no-op request must not introduce the optional fields.
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "response_format") || strings.Contains(string(raw), "grammar") {
		t.Errorf("no-op request leaked constraint fields: %s", raw)
	}
}

// TestApplyConstraintJSONSchemaPreferredOverGrammar: when both are advertised,
// json_schema wins.
func TestApplyConstraintPrefersJSONSchema(t *testing.T) {
	ws, _ := wsAt(t)
	reg := DefaultRegistry(ws)
	req := ApplyConstraint(llm.ChatRequest{}, reg, EndpointCaps{SupportsJSONSchema: true, SupportsGrammar: true})
	if req.ResponseFormat == nil || req.Grammar != "" {
		t.Errorf("json_schema should win over grammar when both supported")
	}
}
