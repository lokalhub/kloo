package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchFindsMatchesSkipsNoise(t *testing.T) {
	ws, root := wsAt(t)
	mk := func(rel, content string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	mk("src/a.ts", "export class TabsPage {}\nconst x = 1\n")
	mk("src/b.ts", "import { TabsPage } from './a'\n")
	mk("node_modules/dep/i.js", "TabsPage everywhere\n") // skipped dir

	res, err := searchTool{ws}.Invoke(context.Background(), Call{Name: NameSearch, Args: map[string]any{"query": "TabsPage"}})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := res.Output
	for _, want := range []string{"src/a.ts:1:", "src/b.ts:1:", "found 2 match"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "node_modules") {
		t.Error("node_modules must be skipped")
	}

	// no matches → clear message, no error
	res2, err := searchTool{ws}.Invoke(context.Background(), Call{Name: NameSearch, Args: map[string]any{"query": "Nonexistent"}})
	if err != nil || !strings.Contains(res2.Output, "no matches") {
		t.Errorf("no-match: out=%q err=%v", res2.Output, err)
	}

	// invalid regex → error
	if _, err := (searchTool{ws}).Invoke(context.Background(), Call{Name: NameSearch, Args: map[string]any{"query": "["}}); err == nil {
		t.Error("invalid regex should error")
	}

	// path-limited + case-insensitive
	res3, _ := searchTool{ws}.Invoke(context.Background(), Call{Name: NameSearch, Args: map[string]any{"query": "(?i)tabspage", "path": "src/b.ts"}})
	if !strings.Contains(res3.Output, "src/b.ts:1:") || strings.Contains(res3.Output, "src/a.ts") {
		t.Errorf("path-limit/case-insensitive failed: %q", res3.Output)
	}
}
