package tools

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/lokalhub/kloo/internal/repomap"
)

// NameReadDir is the read_dir tool name.
const NameReadDir = "read_dir"

// readDirMaxOutput bounds the total bytes one read_dir call returns, so it can't
// blow the context window even on a large folder. Generous (read_dir is for
// big-context models that want a whole area at once), but bounded — the rest is
// reported and reachable via read_file.
const readDirMaxOutput = 128 * 1024 // 128 KiB

// readDirTool is the read_dir tool: it reads the text contents of every file in a
// folder (recursively) in ONE call, so a big-context model can review a whole area
// without N separate read_file round-trips. It reuses repomap.Walk, so it skips the
// same dependency/build dirs the repo map does (node_modules, dist, build, www, …)
// and honours .gitignore at every level; binary and oversize files are skipped.
type readDirTool struct{ ws Workspace }

func (t readDirTool) Name() string { return NameReadDir }
func (t readDirTool) Description() string {
	return "Read the text contents of every file in a folder (recursively) in one call, so you can review a whole area at once instead of many read_file calls. Skips dependency/build dirs (node_modules, dist, build, …) and .gitignored files. Output is bounded; if it reports truncation, read the remaining files individually with read_file."
}
func (t readDirTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{
			"path": {Type: "string", Description: "Workspace-relative folder path (\".\" for the workspace root)."},
		},
		Required: []string{"path"},
	}
}

func (t readDirTool) Invoke(ctx context.Context, c Call) (Result, error) {
	rel, _ := argString(c.Args, "path")
	if strings.TrimSpace(rel) == "" {
		rel = "."
	}
	abs, err := t.ws.Resolve(rel)
	if err != nil {
		return Result{}, err // ErrPathEscape surfaces unchanged
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Result{}, fmt.Errorf("tools: read_dir %s: %w", rel, err)
	}
	if !info.IsDir() {
		return Result{}, fmt.Errorf("tools: read_dir %s: not a folder — use read_file for a single file", rel)
	}

	// repomap.Walk gives a deterministic, skip-aware, .gitignore-honouring file list
	// (and skips files >1 MiB), the same set the repo map sees.
	nodes, err := repomap.Walk(abs)
	if err != nil {
		return Result{}, fmt.Errorf("tools: read_dir %s: %w", rel, err)
	}
	files := repomap.Files(nodes)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	var body strings.Builder
	read, skipped, total, truncatedAt := 0, 0, 0, -1
	for i, f := range files {
		relToWs := f.Path
		if rel != "." {
			relToWs = path.Join(rel, f.Path)
		}
		content, rerr := ReadFile(t.ws, relToWs)
		if rerr != nil { // oversize / unreadable — skip, reachable via read_file
			skipped++
			continue
		}
		// Skip binary blobs (null byte or invalid UTF-8): they're noise in context.
		if strings.IndexByte(content, 0) >= 0 || !utf8.ValidString(content) {
			skipped++
			continue
		}
		chunk := "===== " + relToWs + " =====\n" + content
		if !strings.HasSuffix(chunk, "\n") {
			chunk += "\n"
		}
		chunk += "\n"
		if total+len(chunk) > readDirMaxOutput && read > 0 {
			truncatedAt = i
			break
		}
		body.WriteString(chunk)
		total += len(chunk)
		read++
	}

	var head strings.Builder
	fmt.Fprintf(&head, "read %d file(s) from %s", read, rel)
	if skipped > 0 {
		fmt.Fprintf(&head, " (%d skipped: binary/oversize)", skipped)
	}
	if truncatedAt >= 0 {
		fmt.Fprintf(&head, " — TRUNCATED: %d more file(s) not shown (output budget reached); read them individually with read_file", len(files)-truncatedAt)
	}
	if read == 0 {
		head.WriteString("\n(no readable text files here)")
	}
	head.WriteString("\n\n")
	return Result{Output: head.String() + body.String()}, nil
}
