package tools

import (
	"context"
	"strings"
	"testing"
)

func todoCall(items []any, merge any) Call {
	a := map[string]any{"todos": items}
	if merge != nil {
		a["merge"] = merge
	}
	return Call{Name: NameTodoWrite, Args: a}
}

// TestTodoWriteMergesById: grok's semantics — send only what changed, and a status
// flip needs just id + status.
func TestTodoWriteMergesById(t *testing.T) {
	l := NewTodoList()
	tw := todoWriteTool{l}
	ctx := context.Background()
	if _, err := tw.Invoke(ctx, todoCall([]any{
		map[string]any{"id": "1", "content": "find the failing assertion", "status": "pending"},
		map[string]any{"id": "2", "content": "fix the reconciler", "status": "pending"},
	}, nil)); err != nil {
		t.Fatal(err)
	}
	// status-only update must keep the content
	if _, err := tw.Invoke(ctx, todoCall([]any{
		map[string]any{"id": "1", "status": "completed"},
	}, nil)); err != nil {
		t.Fatal(err)
	}
	got := l.Render()
	if !strings.Contains(got, "[x] find the failing assertion") {
		t.Fatalf("status-only merge lost the content: %q", got)
	}
	if !strings.Contains(got, "[ ] fix the reconciler") {
		t.Fatalf("merge dropped an untouched item: %q", got)
	}
}

// TestTodoWriteReplaceMode.
func TestTodoWriteReplaceMode(t *testing.T) {
	l := NewTodoList()
	tw := todoWriteTool{l}
	ctx := context.Background()
	tw.Invoke(ctx, todoCall([]any{map[string]any{"id": "1", "content": "old"}}, nil))
	tw.Invoke(ctx, todoCall([]any{map[string]any{"id": "9", "content": "new"}}, false))
	got := l.Render()
	if strings.Contains(got, "old") || !strings.Contains(got, "new") {
		t.Fatalf("merge=false did not replace: %q", got)
	}
}

// TestTodoWriteRejectsUnusableInput: an item with no id cannot be merged or
// updated later, so silently accepting it would produce a list the model cannot
// modify.
func TestTodoWriteRejectsUnusableInput(t *testing.T) {
	tw := todoWriteTool{NewTodoList()}
	if _, err := tw.Invoke(context.Background(), todoCall([]any{map[string]any{"content": "no id"}}, nil)); err == nil {
		t.Fatal("accepted an item with no id")
	}
}

// TestEmptyListRendersNothing: an unused list must not occupy a pin every turn.
func TestEmptyListRendersNothing(t *testing.T) {
	if NewTodoList().Render() != "" {
		t.Fatal("empty list rendered")
	}
	var nilList *TodoList
	if nilList.Render() != "" {
		t.Fatal("nil list rendered")
	}
}

// TestTodoWriteIsOptIn: the default tool vocabulary must be unchanged.
func TestTodoWriteIsOptIn(t *testing.T) {
	ws, _ := newWS(t)
	t.Setenv("KLOO_TODO_WRITE", "")
	for _, tl := range DefaultRegistry(ws).Tools() {
		if tl.Name() == NameTodoWrite {
			t.Fatal("todo_write offered with the flag off")
		}
	}
	t.Setenv("KLOO_TODO_WRITE", "1")
	found := false
	reg := DefaultRegistry(ws)
	for _, tl := range reg.Tools() {
		if tl.Name() == NameTodoWrite {
			found = true
		}
	}
	if !found {
		t.Fatal("todo_write not offered with the flag on")
	}
	if reg.Todos() == nil {
		t.Fatal("registry exposes no list for the loop to pin")
	}
}
