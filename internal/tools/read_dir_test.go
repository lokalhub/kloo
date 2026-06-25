package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadDirBulkReadsTextFilesSkipsNoise(t *testing.T) {
	ws, root := wsAt(t)
	mk := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("src/a.ts", "export const a = 1\n")
	mk("src/b.ts", "export const b = 2\n")
	mk("src/node_modules/dep/index.js", "module.exports={}\n") // skipped (dep dir)
	mk("src/logo.png", "\x89PNG\x00\x00binary\x00bytes")       // skipped (binary)

	res, err := readDirTool{ws}.Invoke(context.Background(), Call{Name: NameReadDir, Args: map[string]any{"path": "src"}})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := res.Output
	// Both text files present, with workspace-relative path headers.
	for _, want := range []string{"src/a.ts", "export const a = 1", "src/b.ts", "export const b = 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
	// node_modules content and the binary must NOT be dumped (the binary's path won't
	// appear as a header when skipped).
	if strings.Contains(out, "module.exports") {
		t.Error("node_modules content should be skipped")
	}
	if strings.Contains(out, "logo.png") {
		t.Error("binary file should be skipped (no header for it)")
	}
	// Header reports a count.
	if !strings.HasPrefix(out, "read 2 file") {
		t.Errorf("expected a 'read 2 file(s)' header, got: %q", strings.SplitN(out, "\n", 2)[0])
	}
}

func TestReadDirRejectsAFile(t *testing.T) {
	ws, root := wsAt(t)
	if err := os.WriteFile(filepath.Join(root, "x.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := readDirTool{ws}.Invoke(context.Background(), Call{Name: NameReadDir, Args: map[string]any{"path": "x.txt"}})
	if err == nil || !strings.Contains(err.Error(), "not a folder") {
		t.Errorf("read_dir on a file should error 'not a folder', got %v", err)
	}
}
