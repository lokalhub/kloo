package tools

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
)

// NameViewImage is the view_image tool: it hands the model an image FILE from the
// workspace as a vision content part.
const NameViewImage = "view_image"

// maxViewImageBytes caps one viewed image, measured on the file. The data URL is
// ~4/3 of this, which lands on the same 5 MiB ceiling the TUI's image paste uses
// (internal/tui.imagePasteMax) — a generous full-screen screenshot. Past that the
// request body itself becomes the problem.
const maxViewImageBytes = 3 << 20 // 3 MiB on disk ⇒ ~4 MiB as base64

// viewImageTool lets the agent LOOK at an image it produced itself.
//
// Before this tool, vision was reachable only when a HUMAN pasted an image into
// the TUI: llm.Message.Images was populated from Loop.TaskImages and nowhere else,
// and tools.Result had no image field at all. So an agent could run a screenshot
// command perfectly well and the PNG it had just written was invisible to it — the
// one thing it could not do was look at its own work. On a vision-capable model
// (qwen3.8-flash-next and glm-5.3-flash both read a rendered app screenshot
// accurately in testing) that was the whole difference between measuring a layout
// and seeing it.
//
// It is a separate tool rather than an extension of read_file because the two
// return different KINDS of thing: read_file returns text the model reads, and a
// binary PNG run through it is either refused or gibberish. Keeping them apart also
// keeps read_file's output contract unchanged.
type viewImageTool struct{ ws Workspace }

func (t viewImageTool) Name() string { return NameViewImage }
func (t viewImageTool) Description() string {
	return "Look at an image file in the workspace (png/jpeg/gif/webp) — a screenshot, " +
		"a rendered page, a diagram. Use this after taking a screenshot with run_command " +
		"to SEE what you built, not just measure it. Cap " + itoa(maxViewImageBytes/(1<<20)) + " MiB per image."
}
func (t viewImageTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{
			"path": {Type: "string", Description: "Workspace-relative path to the image file."},
		},
		Required: []string{"path"},
	}
}

func (t viewImageTool) Invoke(ctx context.Context, c Call) (Result, error) {
	path, _ := argString(c.Args, "path")
	abs, err := t.ws.Resolve(path)
	if err != nil {
		return Result{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Result{}, fmt.Errorf("tools: view_image %s: %w", path, err)
	}
	if info.IsDir() {
		return Result{}, fmt.Errorf("tools: view_image %s: is a directory, not an image", path)
	}
	if info.Size() > maxViewImageBytes {
		return Result{}, fmt.Errorf("tools: view_image %s: file is %d bytes (cap %d) — resize or crop it first", path, info.Size(), maxViewImageBytes)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return Result{}, fmt.Errorf("tools: view_image %s: %w", path, err)
	}
	// The BYTES decide the type, never the extension: a screenshot tool that failed
	// writes an HTML error page or a zero-length file to the .png path it was given,
	// and sending that as image/png makes the endpoint reject the whole request with
	// an error that names nothing useful.
	mime, ok := sniffImageBytes(data)
	if !ok {
		return Result{}, fmt.Errorf("tools: view_image %s: not a png/jpeg/gif/webp image (%d bytes) — check the command that produced it actually succeeded", path, len(data))
	}
	return Result{
		Output: fmt.Sprintf("image %s (%s, %d bytes) attached below — look at it.", path, mime, len(data)),
		Images: []string{"data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)},
	}, nil
}

// sniffImageBytes identifies an image by its magic bytes and returns its MIME type.
// Mirrors internal/tui.sniffImage; duplicated rather than shared because internal/
// tools must not import the TUI, and the whole table is four cases.
func sniffImageBytes(b []byte) (string, bool) {
	switch {
	case len(b) >= 8 && string(b[:8]) == "\x89PNG\r\n\x1a\n":
		return "image/png", true
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg", true
	case len(b) >= 6 && (string(b[:6]) == "GIF87a" || string(b[:6]) == "GIF89a"):
		return "image/gif", true
	case len(b) >= 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp", true
	}
	return "", false
}
