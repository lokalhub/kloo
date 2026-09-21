package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// NameTodoWrite is grok's task-list tool, the one substantive capability in its
// captured 26-tool surface that kloo has no equivalent for.
//
// Everything else grok offers is either irrelevant to a headless fix-the-failing-
// test task (image/video generation, schedulers, web search, plan mode) or already
// present under another name (grep≈search, spawn_subagent≈task,
// run_terminal_command≈run_command, search_replace — measured, no benefit).
//
// grok's description, verbatim: "Create and manage a structured task list. Use for
// any task with 3+ steps. Skip for trivial single-step work."
const NameTodoWrite = "todo_write"

// TodoList is the per-run task list. Safe for the loop's single-goroutine use and
// for a subagent running alongside.
type TodoList struct {
	mu    sync.Mutex
	order []string
	items map[string]todoItem
}

type todoItem struct {
	content string
	status  string
}

func NewTodoList() *TodoList { return &TodoList{items: map[string]todoItem{}} }

// Render returns the list for injection into the prompt, or "" when empty.
func (l *TodoList) Render() string {
	if l == nil {
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.order) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Your task list:\n")
	for _, id := range l.order {
		it := l.items[id]
		mark := map[string]string{
			"completed": "x", "in_progress": ">", "cancelled": "-",
		}[it.status]
		if mark == "" {
			mark = " "
		}
		fmt.Fprintf(&b, "  [%s] %s\n", mark, it.content)
	}
	return b.String()
}

// Done reports whether every item is completed or cancelled (and there is at least
// one). The loop uses it only for accounting; it never decides success — verify
// does.
func (l *TodoList) Done() bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.order) == 0 {
		return false
	}
	for _, id := range l.order {
		switch l.items[id].status {
		case "completed", "cancelled":
		default:
			return false
		}
	}
	return true
}

type todoWriteTool struct{ list *TodoList }

func (t todoWriteTool) Name() string { return NameTodoWrite }
func (t todoWriteTool) Description() string {
	return "Create and manage a structured task list for this run. Use it for work with 3 or more steps; " +
		"skip it for a trivial single-step change. Send only the items you are changing — they are merged by " +
		"id. Status is one of: pending, in_progress, completed, cancelled. The list does NOT decide success; " +
		"the verify command does."
}
func (t todoWriteTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{
			"todos": {Type: "array", Description: "Items to write, each {id, content, status}. Merged by id unless merge is false."},
			"merge": {Type: "boolean", Description: "Merge into the existing list by id (default true); false replaces it."},
		},
		Required: []string{"todos"},
	}
}

func (t todoWriteTool) Invoke(ctx context.Context, c Call) (Result, error) {
	raw, ok := c.Args["todos"].([]any)
	if !ok {
		return Result{}, fmt.Errorf("tools: todo_write: todos must be an array")
	}
	merge := true
	if m, ok := c.Args["merge"].(bool); ok {
		merge = m
	}
	t.list.mu.Lock()
	if !merge {
		t.list.order, t.list.items = nil, map[string]todoItem{}
	}
	changed := 0
	for _, e := range raw {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if strings.TrimSpace(id) == "" {
			continue
		}
		prev := t.list.items[id]
		it := prev
		if s, ok := m["content"].(string); ok && s != "" {
			it.content = s
		}
		if s, ok := m["status"].(string); ok && s != "" {
			it.status = s
		}
		if it.status == "" {
			it.status = "pending"
		}
		if it.content == "" {
			it.content = id // never render a blank row
		}
		if _, existed := t.list.items[id]; !existed {
			t.list.order = append(t.list.order, id)
		}
		t.list.items[id] = it
		changed++
	}
	sort.SliceStable(t.list.order, func(i, j int) bool { return false }) // insertion order
	out := t.list.renderLocked()
	t.list.mu.Unlock()
	if changed == 0 {
		return Result{}, fmt.Errorf("tools: todo_write: no usable items (each needs an id)")
	}
	return Result{Output: out}, nil
}

func (l *TodoList) renderLocked() string {
	var b strings.Builder
	b.WriteString("task list updated:\n")
	for _, id := range l.order {
		it := l.items[id]
		b.WriteString("  " + it.status + ": " + it.content + "\n")
	}
	return b.String()
}
