package mcp

import (
	"fmt"
	"hash/fnv"
	"sort"

	"github.com/lokalhub/kloo/internal/tools"
)

// maxToolNameLen is the OpenAI function-name length cap; kloo-facing MCP tool
// names are truncated to fit it (and the broader native-FC charset).
const maxToolNameLen = 64

// toParamSchema maps an MCP tool's InputSchema — a JSON-Schema 2020-12 object seen
// client-side as map[string]any — to kloo's flat tools.ParamSchema. Each
// properties.<k>.{type,description} becomes a tools.Property; required[] becomes
// Required. Nested object/array property types keep their JSON-Schema "type"
// string verbatim (kloo's ParamSchema is intentionally flat; the model's full
// args still pass through untouched to CallTool). A nil, non-object, or
// properties-less schema yields an empty ParamSchema (the tool takes no/free-form
// args).
func toParamSchema(input any) tools.ParamSchema {
	m, ok := input.(map[string]any)
	if !ok || m == nil {
		return tools.ParamSchema{}
	}
	props, ok := m["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return tools.ParamSchema{}
	}

	schema := tools.ParamSchema{Properties: make(map[string]tools.Property, len(props))}
	for name, raw := range props {
		pm, ok := raw.(map[string]any)
		if !ok {
			// A malformed property entry: record it with an empty type rather than
			// dropping it, so the tool's parameter is still surfaced to the model.
			schema.Properties[name] = tools.Property{}
			continue
		}
		typ, _ := pm["type"].(string) // missing/array-typed ⇒ "" (kept verbatim otherwise)
		desc, _ := pm["description"].(string)
		schema.Properties[name] = tools.Property{Type: typ, Description: desc}
	}

	if req, ok := m["required"].([]any); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				schema.Required = append(schema.Required, s)
			}
		}
		sort.Strings(schema.Required) // deterministic
	}
	return schema
}

// toolName produces a kloo-facing, adapter-legal name for a server's tool:
// "<server>__<tool>", sanitized to the OpenAI function-name charset
// ^[a-zA-Z0-9_-]{1,64}$ (so '.', spaces, and unicode become '_'), capped at 64
// chars. It is deterministic. When the sanitized join exceeds 64 chars it is
// truncated with a short numeric suffix derived from a hash of the full name, so
// two distinct tools that share a 64-char prefix still map to distinct names.
func toolName(server, tool string) string {
	s := sanitizeName(server + "__" + tool)
	if len(s) <= maxToolNameLen {
		return s
	}
	// Truncate, reserving a deterministic numeric suffix that disambiguates
	// collisions on the truncated prefix.
	const suffixLen = 7 // '_' + 6 digits
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	suffix := fmt.Sprintf("_%06d", h.Sum32()%1_000_000)
	return s[:maxToolNameLen-suffixLen] + suffix
}

// sanitizeName replaces every character outside [a-zA-Z0-9_-] with '_'. An empty
// result becomes "_" so the name always satisfies the {1,64} minimum.
func sanitizeName(s string) string {
	b := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b = append(b, byte(r))
		default:
			b = append(b, '_')
		}
	}
	if len(b) == 0 {
		return "_"
	}
	return string(b)
}
