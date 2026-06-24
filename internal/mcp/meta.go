package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/lokalhub/kloo/internal/tools"
)

// meta.go implements the lazy meta-tool trio that makes a large MCP server
// (≈33 tools) window-safe by default: instead of dumping every tool's schema into
// every turn, a lazy server registers exactly three small tools — list, describe,
// call — so its window cost is constant (~3 schemas) regardless of server size
// (master plan §5). The model's flow is: list_tools → (describe_tool) → call_tool.

// list_tools output budget — kept small so the RESULT itself never floods a small
// model's window. Overflow is paginated behind a cursor, never silently dropped.
const (
	metaListMaxTools = 50
	metaListMaxChars = 1500
)

// ── <server>__list_tools ─────────────────────────────────────────────────────

type listToolsMetaTool struct {
	name   string
	client *Client
}

func (t *listToolsMetaTool) Name() string { return t.name }
func (t *listToolsMetaTool) Description() string {
	return "List this MCP server's tools (names + one-line summaries) cheaply. " +
		"Step 1 of 3: list_tools → describe_tool{name} for one tool's full schema → call_tool{name,arguments} to run it. " +
		"Pass {cursor} from a previous result to see more."
}
func (t *listToolsMetaTool) Schema() tools.ParamSchema {
	return tools.ParamSchema{
		Properties: map[string]tools.Property{
			"cursor": {Type: "string", Description: "Optional pagination cursor from a previous list_tools result."},
		},
	}
}
func (t *listToolsMetaTool) Invoke(_ context.Context, c tools.Call) (tools.Result, error) {
	start := 0
	if cur := metaArgString(c.Args, "cursor"); cur != "" {
		if n, err := strconv.Atoi(cur); err == nil && n > 0 {
			start = n
		}
	}

	all := t.client.tools
	var b strings.Builder
	i := start
	for ; i < len(all); i++ {
		line := all[i].Name + " — " + firstLine(all[i].Description) + "\n"
		// Stop before exceeding either cap (always emit at least one line).
		if i > start && (i-start >= metaListMaxTools || b.Len()+len(line) > metaListMaxChars) {
			break
		}
		b.WriteString(line)
	}
	if i < len(all) {
		fmt.Fprintf(&b, "… %d more; call %s again with cursor=%q\n", len(all)-i, t.name, strconv.Itoa(i))
	}
	if b.Len() == 0 {
		b.WriteString("(this server advertises no tools)\n")
	}
	return tools.Result{Output: b.String()}, nil
}

// ── <server>__describe_tool ──────────────────────────────────────────────────

type describeToolMetaTool struct {
	name   string
	client *Client
}

func (t *describeToolMetaTool) Name() string { return t.name }
func (t *describeToolMetaTool) Description() string {
	return "Show the full JSON schema for ONE tool on this MCP server. " +
		"Step 2 of 3: call after list_tools, before call_tool. Args: {name}."
}
func (t *describeToolMetaTool) Schema() tools.ParamSchema {
	return tools.ParamSchema{
		Properties: map[string]tools.Property{
			"name": {Type: "string", Description: "The tool name from list_tools."},
		},
		Required: []string{"name"},
	}
}
func (t *describeToolMetaTool) Invoke(_ context.Context, c tools.Call) (tools.Result, error) {
	name := metaArgString(c.Args, "name")
	if name == "" {
		return tools.Result{Output: "describe_tool needs a 'name'; call " + listName(t.name) + " to see tool names."}, nil
	}
	mt, ok := toolByName(t.client, name)
	if !ok {
		return tools.Result{Output: fmt.Sprintf("no such tool: %s; call %s to see available tools.", name, listName(t.name))}, nil
	}
	schema, err := json.MarshalIndent(mt.InputSchema, "", "  ")
	if err != nil {
		schema = []byte(fmt.Sprintf("%v", mt.InputSchema))
	}
	var b strings.Builder
	b.WriteString(mt.Name)
	if mt.Description != "" {
		b.WriteString(" — " + mt.Description)
	}
	b.WriteString("\ninput schema:\n")
	b.Write(schema)
	return tools.Result{Output: b.String()}, nil
}

// ── <server>__call_tool ──────────────────────────────────────────────────────

type callToolMetaTool struct {
	name   string
	client *Client
}

func (t *callToolMetaTool) Name() string { return t.name }
func (t *callToolMetaTool) Description() string {
	return "Invoke ANY tool on this MCP server by name. " +
		"Step 3 of 3: after list_tools/describe_tool. Args: {name, arguments} where arguments is an object " +
		"of that tool's parameters (a JSON-string object is also accepted)."
}
func (t *callToolMetaTool) Schema() tools.ParamSchema {
	return tools.ParamSchema{
		Properties: map[string]tools.Property{
			"name":      {Type: "string", Description: "The tool name to invoke (from list_tools)."},
			"arguments": {Type: "object", Description: "The tool's arguments as a JSON object."},
		},
		Required: []string{"name"},
	}
}
func (t *callToolMetaTool) Invoke(ctx context.Context, c tools.Call) (tools.Result, error) {
	name := metaArgString(c.Args, "name")
	if name == "" {
		return tools.Result{}, fmt.Errorf("call_tool needs a 'name'; call %s to see tool names", listName(t.name))
	}
	if _, ok := toolByName(t.client, name); !ok {
		return tools.Result{}, fmt.Errorf("no such tool: %s; call %s to see available tools", name, listName(t.name))
	}
	args := decodeMetaArgs(c.Args["arguments"])
	return invokeRemote(ctx, t.client, name, t.name+":"+name, args)
}

// ── registration + helpers ───────────────────────────────────────────────────

// registerMetaTrio registers the three namespaced meta-tools for a lazy server
// and returns their kloo-facing names. Constant window cost (3 tools) regardless
// of how many tools the server advertises.
func registerMetaTrio(reg *tools.Registry, c *Client) []string {
	trio := []tools.Tool{
		&listToolsMetaTool{name: toolName(c.Name, "list_tools"), client: c},
		&describeToolMetaTool{name: toolName(c.Name, "describe_tool"), client: c},
		&callToolMetaTool{name: toolName(c.Name, "call_tool"), client: c},
	}
	names := make([]string, 0, len(trio))
	for _, t := range trio {
		reg.Register(t)
		names = append(names, t.Name())
	}
	return names
}

// metaArgString reads a string arg (mirrors tools.argString, which is unexported).
func metaArgString(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

// decodeMetaArgs tolerates `arguments` given either as a JSON object or as a
// JSON-string of an object (the lenient style from internal/tools/json_text.go),
// returning an empty map for anything else.
func decodeMetaArgs(v any) map[string]any {
	switch a := v.(type) {
	case map[string]any:
		return a
	case string:
		m := map[string]any{}
		_ = json.Unmarshal([]byte(a), &m)
		return m
	default:
		return map[string]any{}
	}
}

// firstLine returns the first line of s (the one-line summary for list_tools).
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// listName derives the sibling list_tools name from any meta-tool's namespaced
// name (they share the "<server>__" prefix), for cross-referencing in messages.
func listName(metaName string) string {
	if i := strings.LastIndex(metaName, "__"); i >= 0 {
		return metaName[:i] + "__list_tools"
	}
	return "list_tools"
}
