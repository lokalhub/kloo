package tools

import (
	"context"
	"fmt"
	"strings"
)

// This file wires the Phase-01 file functions (ReadFile/ListDir/WriteFile/
// EditFile in files.go) into Tool implementations registered in the vocabulary.
// It does not re-implement file I/O or path safety — every tool resolves through
// the same Workspace jail the underlying functions use.

// readFileTool is the read_file tool.
type readFileTool struct{ ws Workspace }

func (t readFileTool) Name() string { return NameReadFile }
func (t readFileTool) Description() string {
	return "Read the full contents of a file in the workspace."
}
func (t readFileTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{"path": {Type: "string", Description: "Workspace-relative path to the file."}},
		Required:   []string{"path"},
	}
}
func (t readFileTool) Invoke(ctx context.Context, c Call) (Result, error) {
	path, _ := argString(c.Args, "path")
	content, err := ReadFile(t.ws, path)
	if err != nil {
		return Result{}, err
	}
	// An empty (or whitespace-only) file would otherwise return a BLANK observation,
	// which a small model can't tell apart from "the read gave me nothing" — and it
	// loops, re-reading the same file forever ("let me check the content…"). Return
	// an explicit marker so the model knows the file IS empty and moves on.
	if strings.TrimSpace(content) == "" {
		return Result{Output: "(file exists but is empty — 0 meaningful bytes)"}, nil
	}
	return Result{Output: content}, nil
}

// listDirTool is the list_dir tool.
type listDirTool struct{ ws Workspace }

func (t listDirTool) Name() string        { return NameListDir }
func (t listDirTool) Description() string { return "List the entries of a directory in the workspace." }
func (t listDirTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{"path": {Type: "string", Description: "Workspace-relative directory path (\".\" for the root)."}},
		Required:   []string{"path"},
	}
}
func (t listDirTool) Invoke(ctx context.Context, c Call) (Result, error) {
	path, _ := argString(c.Args, "path")
	entries, err := ListDir(t.ws, path)
	if err != nil {
		return Result{}, err
	}
	var b strings.Builder
	for _, e := range entries {
		if e.IsDir {
			b.WriteString(e.Name + "/\n")
		} else {
			b.WriteString(e.Name + "\n")
		}
	}
	return Result{Output: b.String()}, nil
}

// writeFileTool is the write_file tool (full-content write; may overwrite).
type writeFileTool struct{ ws Workspace }

func (t writeFileTool) Name() string { return NameWriteFile }
func (t writeFileTool) Description() string {
	return "Write full content to a file (creating or overwriting it). Prefer edit_file for changes to existing files."
}
func (t writeFileTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{
			"path":    {Type: "string", Description: "Workspace-relative path to write."},
			"content": {Type: "string", Description: "The full file content."},
		},
		Required: []string{"path", "content"},
	}
}
func (t writeFileTool) Invoke(ctx context.Context, c Call) (Result, error) {
	path, _ := argString(c.Args, "path")
	content, _ := argString(c.Args, "content")
	if err := WriteFile(t.ws, path, content); err != nil {
		return Result{}, err
	}
	return Result{Output: fmt.Sprintf("wrote %s (%d bytes)", path, len(content))}, nil
}

// editFileTool is the edit_file tool: it wraps the Phase-01 SEARCH/REPLACE
// engine via EditFile. The diff arg carries fenced SEARCH/REPLACE block(s); the
// path is a separate arg, so the tool prefixes the path as the engine's filename
// line (decisions.md). Engine errors (ErrSearchNotFound / ErrMalformedBlock)
// surface unchanged.
type editFileTool struct{ ws Workspace }

func (t editFileTool) Name() string { return NameEditFile }
func (t editFileTool) Description() string {
	return "Edit a file by applying fenced SEARCH/REPLACE blocks. The SEARCH text must match exactly. This is the preferred way to change existing files."
}
func (t editFileTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{
			"path": {Type: "string", Description: "Workspace-relative path to edit."},
			"diff": {Type: "string", Description: "A SEARCH/REPLACE block with ALL THREE marker lines, each on its own line, in this EXACT order — the ======= divider line between the two sections is REQUIRED (do not omit it or replace it with >>>>>>> REPLACE):\n<<<<<<< SEARCH\n<exact lines to find>\n=======\n<replacement lines>\n>>>>>>> REPLACE\nThe SEARCH text must match the file byte-for-byte. To create a new file, leave the SEARCH section empty. The ``` fence is optional."},
		},
		Required: []string{"path", "diff"},
	}
}
func (t editFileTool) Invoke(ctx context.Context, c Call) (Result, error) {
	path, _ := argString(c.Args, "path")
	diff, _ := argString(c.Args, "diff")
	// The path is a separate arg; prefix it as the engine's filename line so the
	// bare fenced diff parses. If the diff already carries a filename line, the
	// prefixed line is harmless prose above it (EditFile retargets to path).
	blockText := path + "\n" + diff
	if err := EditFile(t.ws, path, blockText); err != nil {
		return Result{}, err
	}
	return Result{Output: "edited " + path}, nil
}

// DefaultRegistry builds the registry with the five-tool vocabulary, all jailed
// to ws. opts configure run_command (timeout, output bound).
func DefaultRegistry(ws Workspace, opts ...RunCommandOption) *Registry {
	r := NewRegistry()
	r.Register(readFileTool{ws})
	r.Register(readDirTool{ws})
	r.Register(searchTool{ws})
	r.Register(editFileTool{ws})
	r.Register(writeFileTool{ws})
	r.Register(listDirTool{ws})
	r.Register(NewRunCommandTool(ws, opts...))
	r.Register(finishTool{}) // explicit terminator; the loop intercepts it
	return r
}
