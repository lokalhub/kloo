package tools

import (
	"errors"
	"fmt"
)

// ErrUnsupportedToolFormat is returned for an unknown/invalid toolFormat,
// including "yaml" — YAML is never a selectable tool format.
var ErrUnsupportedToolFormat = errors.New("tools: unsupported toolFormat")

// Tool-format values accepted in a profile's toolFormat (naming.md config keys).
const (
	ToolFormatAuto    = "" // unset/auto → capability-driven
	ToolFormatNative  = "native"
	ToolFormatTrained = "trained"
	ToolFormatXML     = "xml"
)

// EndpointCaps describes what the configured endpoint advertises. For v1 these
// are populated conservatively from the profile (auto-probing is post-v1). The
// grammar/json_schema caps drive the optional constrained-decoding layer
// (constrain.go).
type EndpointCaps struct {
	SupportsTools      bool
	SupportsGrammar    bool
	SupportsJSONSchema bool
}

// SelectAdapter chooses the ToolAdapter for a model from its resolved
// toolFormat and the endpoint capabilities. Precedence (decisions.md):
//
//  1. Explicit toolFormat override wins: "native" | "trained" | "xml". A
//     "xml" override forces the XML adapter even on a tool-capable endpoint.
//  2. Auto (toolFormat unset) → native FC when the endpoint advertises tools,
//     else the XML fallback.
//  3. "yaml" or any unknown value → ErrUnsupportedToolFormat (YAML never
//     selects an adapter).
//
// "trained" maps onto native FC for v1 (a model's trained tool-call format runs
// through the native function-calling path); recorded in decisions.md.
//
// Note: the prompt named this parameter config.Profile; config exposes no such
// type, so the operative field — the resolved toolFormat string — is passed
// directly, keeping internal/tools decoupled from internal/config (decisions.md).
func SelectAdapter(toolFormat string, caps EndpointCaps) (ToolAdapter, error) {
	switch toolFormat {
	case ToolFormatNative, ToolFormatTrained:
		return NativeFCAdapter{}, nil
	case ToolFormatXML:
		return XMLAdapter{}, nil
	case ToolFormatAuto:
		if caps.SupportsTools {
			return NativeFCAdapter{}, nil
		}
		return XMLAdapter{}, nil
	default:
		return nil, fmt.Errorf("tools: toolFormat %q (yaml is never selectable): %w", toolFormat, ErrUnsupportedToolFormat)
	}
}
