package tools

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// Tool-vocabulary sentinel errors. Adapters and the loop match these with
// errors.Is; the re-prompt rail (reprompt.go) turns the parse-level ones into a
// single corrective re-prompt.
var (
	// ErrUnknownTool is returned by Dispatch when a Call names a tool the
	// registry does not have.
	ErrUnknownTool = errors.New("tools: unknown tool")
	// ErrNoToolCall is returned when a turn yielded zero tool calls.
	ErrNoToolCall = errors.New("tools: no tool call in assistant turn")
	// ErrMultipleToolCalls is returned when a turn yielded more than one tool
	// call (one-tool-per-turn is enforced; never silently pick the first).
	ErrMultipleToolCalls = errors.New("tools: more than one tool call in assistant turn")
	// ErrMalformedToolCall is returned when a tool call's arguments could not be
	// parsed (e.g. invalid JSON in a native tool_call, or a broken XML block).
	ErrMalformedToolCall = errors.New("tools: malformed tool call")
	// ErrInvalidArgs is returned when a Call is missing a required argument for
	// its tool's schema.
	ErrInvalidArgs = errors.New("tools: invalid tool arguments")
)

// Property is one parameter in a tool's JSON-schema (type + description).
type Property struct {
	Type        string
	Description string
}

// ParamSchema is a minimal JSON-schema description of a tool's parameters: an
// object with named properties and a required list. It serialises to the OpenAI
// function.parameters shape (used by the native-FC adapter) and is the single
// source the constrained-decoding layer derives its constraint from.
type ParamSchema struct {
	Properties map[string]Property
	Required   []string
}

// JSONSchema renders the schema to the wire object
// {type:"object", properties:{…}, required:[…]} with deterministic key order.
func (s ParamSchema) JSONSchema() map[string]any {
	props := make(map[string]any, len(s.Properties))
	for name, p := range s.Properties {
		props[name] = map[string]any{"type": p.Type, "description": p.Description}
	}
	out := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(s.Required) > 0 {
		req := append([]string(nil), s.Required...)
		sort.Strings(req)
		out["required"] = req
	}
	return out
}

// Validate checks that every required property is present in args.
func (s ParamSchema) Validate(args map[string]any) error {
	for _, r := range s.Required {
		if _, ok := args[r]; !ok {
			return fmt.Errorf("tools: missing required argument %q: %w", r, ErrInvalidArgs)
		}
	}
	return nil
}

// Call is the model-agnostic dispatchable tool call. Both the native-FC adapter
// and the XML adapter normalise the model's reply to this single shape, so both
// reach the same handler (proven in the consolidated dispatch test).
type Call struct {
	Name string
	Args map[string]any
}

// Result is a tool's captured output handed back to the loop. Output holds the
// primary text (file content, listing, command stdout, or a confirmation);
// Stderr/ExitCode/TimedOut/Truncated are meaningful for run_command.
type Result struct {
	Output    string
	Stderr    string
	ExitCode  int
	TimedOut  bool
	Truncated bool
}

// Tool is one entry in the agent's vocabulary. The interface is named by
// behaviour (naming.md); concrete tools delegate to the Phase-01 file functions
// or, for run_command, to the exec path.
type Tool interface {
	Name() string
	Description() string
	Schema() ParamSchema
	Invoke(ctx context.Context, c Call) (Result, error)
}

// Registry holds the tool vocabulary and routes a parsed Call to its handler.
type Registry struct {
	order []string
	tools map[string]Tool
	bg    *BackgroundManager // long-running commands started this run; nil ⇒ none
}

// StopBackground kills every background command started through this registry. The
// loop calls it on run termination so a server the agent started never leaks past
// the run. Safe on a nil registry / when no background manager is wired.
func (r *Registry) StopBackground() {
	if r != nil && r.bg != nil {
		r.bg.StopAll()
	}
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]Tool)}
}

// Register adds a tool (last registration of a name wins; order preserved on
// first registration).
func (r *Registry) Register(t Tool) {
	if _, exists := r.tools[t.Name()]; !exists {
		r.order = append(r.order, t.Name())
	}
	r.tools[t.Name()] = t
}

// registerHidden makes t dispatchable (Lookup/Dispatch find it) WITHOUT adding it
// to the advertised vocabulary (Tools() / the OpenAI tools param). It is used to
// withhold the model-facing run_command in scoped / patch-only runs while still
// rejecting a fallback-adapter run_command call before execution (disabled_shell.go),
// instead of surfacing a generic unknown-tool error. Last write wins; a name here
// never appears in `order`.
func (r *Registry) registerHidden(t Tool) {
	delete(r.tools, t.Name())
	r.tools[t.Name()] = t
}

// Lookup returns the tool registered under name.
func (r *Registry) Lookup(name string) (Tool, bool) {
	t, ok := r.tools[name]
	return t, ok
}

// Tools returns the registered tools in stable registration order (used for the
// OpenAI tools param so requests are deterministic).
func (r *Registry) Tools() []Tool {
	out := make([]Tool, 0, len(r.order))
	for _, name := range r.order {
		out = append(out, r.tools[name])
	}
	return out
}

// Dispatch routes a parsed Call to its tool after validating required args.
// An unknown tool name returns ErrUnknownTool; a missing required arg returns
// ErrInvalidArgs. The tool's own Invoke error (or success) is returned otherwise.
func (r *Registry) Dispatch(ctx context.Context, c Call) (Result, error) {
	t, ok := r.tools[c.Name]
	if !ok {
		return Result{}, fmt.Errorf("tools: %q: %w", c.Name, ErrUnknownTool)
	}
	if err := t.Schema().Validate(c.Args); err != nil {
		return Result{}, err
	}
	return t.Invoke(ctx, c)
}

// ExactlyOneCall enforces the one-tool-per-turn rule: zero calls → ErrNoToolCall,
// more than one → ErrMultipleToolCalls, exactly one → that call. Adapters use
// this so a turn never silently picks the first of several calls.
func ExactlyOneCall(calls []Call) (Call, error) {
	switch len(calls) {
	case 0:
		return Call{}, ErrNoToolCall
	case 1:
		return calls[0], nil
	default:
		return Call{}, ErrMultipleToolCalls
	}
}

// argString reads a string argument from a Call's args.
func argString(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}
