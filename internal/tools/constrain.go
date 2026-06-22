package tools

import (
	"fmt"
	"strings"

	"github.com/lokal/kloo/internal/llm"
)

// ApplyConstraint is the OPTIONAL, best-effort constrained-decoding layer. When
// the endpoint advertises support it attaches a generation-time constraint
// derived from the registry's ParamSchema (an OpenAI json_schema response_format
// when SupportsJSONSchema, else a llama.cpp GBNF grammar when SupportsGrammar)
// so the model is decoded into a valid tool-call shape. When neither is
// supported it GRACEFULLY NO-OPS — the request is returned unchanged and the run
// relies on parse + one-re-prompt (reprompt.go), behaving identically to an
// unconstrained endpoint.
//
// The constraint is advisory hardening only: parsing and the re-prompt rail
// still run regardless. The schema/grammar is generated from the existing
// ParamSchema and XML grammar — there is no second hand-authored copy
// (decisions.md). Documented as optional in the phase overview.
func ApplyConstraint(req llm.ChatRequest, reg *Registry, caps EndpointCaps) llm.ChatRequest {
	switch {
	case caps.SupportsJSONSchema:
		req.ResponseFormat = toolCallJSONSchema(reg)
	case caps.SupportsGrammar:
		req.Grammar = toolCallGBNF(reg)
	default:
		// No-op: unchanged request; parse + one-re-prompt handle validity.
	}
	return req
}

// toolCallJSONSchema builds an OpenAI json_schema response_format that constrains
// the reply to a single tool call: an object {name, arguments} where name is one
// of the registered tools and arguments matches that tool's parameter schema.
// Derived entirely from the registry (no duplicate schema source).
func toolCallJSONSchema(reg *Registry) map[string]any {
	var oneOf []any
	for _, t := range reg.Tools() {
		oneOf = append(oneOf, map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":      map[string]any{"const": t.Name()},
				"arguments": t.Schema().JSONSchema(),
			},
			"required":             []string{"name", "arguments"},
			"additionalProperties": false,
		})
	}
	return map[string]any{
		"type": "json_schema",
		"json_schema": map[string]any{
			"name":   "tool_call",
			"strict": true,
			"schema": map[string]any{"oneOf": oneOf},
		},
	}
}

// toolCallGBNF builds a minimal llama.cpp GBNF grammar constraining the reply to
// a JSON object whose "name" is one of the registered tools. The argument bodies
// are left as a permissive JSON object for v1 (best-effort); the tool names are
// derived from the registry, not hand-listed.
func toolCallGBNF(reg *Registry) string {
	var names []string
	for _, t := range reg.Tools() {
		names = append(names, fmt.Sprintf("%q", t.Name()))
	}
	var b strings.Builder
	b.WriteString("root   ::= \"{\" ws \"\\\"name\\\"\" ws \":\" ws name ws \",\" ws \"\\\"arguments\\\"\" ws \":\" ws object ws \"}\"\n")
	b.WriteString("name   ::= " + strings.Join(names, " | ") + "\n")
	b.WriteString("object ::= \"{\" ws ( string ws \":\" ws value (ws \",\" ws string ws \":\" ws value)* )? ws \"}\"\n")
	b.WriteString("value  ::= string | object | \"true\" | \"false\" | \"null\"\n")
	b.WriteString("string ::= \"\\\"\" ([^\"\\\\] | \"\\\\\" .)* \"\\\"\"\n")
	b.WriteString("ws     ::= [ \\t\\n]*\n")
	return b.String()
}
