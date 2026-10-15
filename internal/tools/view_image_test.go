package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// onePNG is the smallest valid PNG: a 1x1 image. Used so the test proves the
// MAGIC-BYTE path, not a filename convention.
var onePNG = []byte{
	0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n',
	0, 0, 0, 0x0d, 'I', 'H', 'D', 'R',
	0, 0, 0, 1, 0, 0, 0, 1, 8, 6, 0, 0, 0,
	0x1f, 0x15, 0xc4, 0x89,
	0, 0, 0, 0x0a, 'I', 'D', 'A', 'T',
	0x78, 0x9c, 0x63, 0, 1, 0, 0, 5, 0, 1,
	0x0d, 0x0a, 0x2d, 0xb4,
	0, 0, 0, 0, 'I', 'E', 'N', 'D', 0xae, 'B', 0x60, 0x82,
}

func viewTool(t *testing.T) (viewImageTool, string) {
	t.Helper()
	dir := t.TempDir()
	ws, err := NewWorkspace(dir)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	return viewImageTool{ws}, dir
}

// A real image comes back as a data URL on Result.Images — never inlined into the
// text Output, where it would be tokenized as gibberish and re-sent every turn.
func TestViewImageReturnsDataURLNotText(t *testing.T) {
	tool, dir := viewTool(t)
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), onePNG, 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Invoke(context.Background(), Call{Name: NameViewImage, Args: map[string]any{"path": "shot.png"}})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(res.Images) != 1 {
		t.Fatalf("want 1 image, got %d", len(res.Images))
	}
	if !strings.HasPrefix(res.Images[0], "data:image/png;base64,") {
		t.Errorf("not a png data URL: %.40q", res.Images[0])
	}
	if strings.Contains(res.Output, "base64") || len(res.Output) > 200 {
		t.Errorf("base64 leaked into the text output: %q", res.Output)
	}
}

// The BYTES decide the type. A screenshot command that failed leaves an HTML error
// page (or nothing) at the .png path it was given; sending that as image/png makes
// the endpoint reject the whole request, so it has to be caught here with an error
// that points at the real cause.
func TestViewImageRefusesNonImageNamedPNG(t *testing.T) {
	tool, dir := viewTool(t)
	if err := os.WriteFile(filepath.Join(dir, "shot.png"), []byte("<html>timeout</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := tool.Invoke(context.Background(), Call{Name: NameViewImage, Args: map[string]any{"path": "shot.png"}})
	if err == nil {
		t.Fatal("want an error for a non-image file named .png")
	}
	if !strings.Contains(err.Error(), "not a png/jpeg/gif/webp") {
		t.Errorf("error does not name the cause: %v", err)
	}
}

func TestViewImageIsRegisteredAndJailed(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewWorkspace(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := DefaultRegistry(ws).Lookup(NameViewImage); !ok {
		t.Fatal("view_image is not in the default vocabulary — the model cannot call it")
	}
	tool := viewImageTool{ws}
	if _, err := tool.Invoke(context.Background(), Call{Name: NameViewImage, Args: map[string]any{"path": "../../etc/passwd"}}); err == nil {
		t.Error("escaped the workspace jail")
	}
}
