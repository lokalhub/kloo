package tools

import (
	"context"
	"fmt"
	"os"
	"strconv"
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
	return "Read a file in the workspace. Returns at most " + itoa(DefaultReadLineLimit) +
		" lines; use offset to page through a longer file, and search to locate what you need first."
}
func (t readFileTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{
			"path":   {Type: "string", Description: "Workspace-relative path to the file."},
			"offset": {Type: "integer", Description: "1-based first line to return. Omit for the start of the file."},
			"limit":  {Type: "integer", Description: "Maximum lines to return (default " + itoa(DefaultReadLineLimit) + ")."},
		},
		Required: []string{"path"},
	}
}
func (t readFileTool) Invoke(ctx context.Context, c Call) (Result, error) {
	path, _ := argString(c.Args, "path")
	content, err := ReadFile(t.ws, path)
	if err != nil {
		return Result{}, err
	}
	// A whole-file dump can exceed the model's entire working budget. Real files in
	// a production repo run to 34-41k TOKENS each, against a hot budget of ~51k at
	// ctx 131072 — so ONE read consumed 80% of it, the next push crossed the
	// compaction trigger, the file was shed, and the model read it again. Measured
	// on kloo-bench: C07 made 46 reads with 18 of them repeats and never edited
	// anything; C65 and A29 died the same way.
	//
	// Returning a bounded window with an explicit marker fixes the cause rather
	// than the symptom: the model can still reach any part of the file, but no
	// single call can swallow the window.
	first := argInt(c.Args, "offset")
	if first < 1 {
		first = 1
	}
	if body, note, truncated := clampLines(content, argInt(c.Args, "offset"), argInt(c.Args, "limit")); truncated {
		if lineAnchors() {
			body = anchorLines(body, first)
		}
		return Result{Output: body + "\n" + note}, nil
	} else {
		if lineAnchors() {
			body = anchorLines(body, first)
		}
		content = body
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

// lineAnchors reports whether read_file annotates its output with grok's
// "LINE_NUMBER→" anchors (KLOO_LINE_ANCHORS=1). Off by default.
func lineAnchors() bool { return envOn("KLOO_LINE_ANCHORS") }

// envOn reports whether a boolean experiment flag is set.
func envOn(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// anchorLines prefixes the first line and every tenth line with "N→", the scheme
// grok's captured read_file uses ("Line numbers (1-based) appear as anchors in the
// format LINE_NUMBER→LINE_CONTENT on the first returned line and on every 10th
// line of the file; the lines in between show content only").
//
// Sparse rather than per-line on purpose: it orients the model in a long file
// without putting a prefix on every line the edit tool will later have to match.
func anchorLines(body string, firstLine int) string {
	lines := strings.Split(body, "\n")
	for i := range lines {
		n := firstLine + i
		if i == 0 || n%10 == 0 {
			lines[i] = strconv.Itoa(n) + "\u2192" + lines[i]
		}
	}
	return strings.Join(lines, "\n")
}

// DefaultReadLineLimit bounds a single read_file result. Chosen so that even a
// dense source file costs a small fraction of the hot budget rather than most of
// it; a model that needs more pages with offset.
const DefaultReadLineLimit = 400

func itoa(n int) string { return strconv.Itoa(n) }

// argInt reads an integer argument, tolerating the float form JSON decoding
// produces and the string form some models emit. 0 when absent or unusable.
func argInt(args map[string]any, key string) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return 0
}

// clampLines returns the requested window of content plus a marker describing what
// was withheld and how to get it. The marker matters as much as the clamp: a
// silently truncated file is a correctness hazard, and kloo has been bitten by
// silent truncation before.
func clampLines(content string, offset, limit int) (body, note string, truncated bool) {
	lines := strings.Split(content, "\n")
	total := len(lines)
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = DefaultReadLineLimit
	}
	start := offset - 1
	if start >= total {
		return "", fmt.Sprintf("(offset %d is past the end; the file has %d lines)", offset, total), true
	}
	end := start + limit
	if end > total {
		end = total
	}
	body = strings.Join(lines[start:end], "\n")
	if start == 0 && end == total {
		return body, "", false
	}
	return body, fmt.Sprintf(
		"\n--- showing lines %d-%d of %d. Use read_file with offset=%d to continue, or search to find a specific symbol. ---",
		start+1, end, total, end+1), true
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
			"diff": {Type: "string", Description: "A SEARCH/REPLACE block with ALL THREE marker lines, each on its own line, in this EXACT order — the ======= divider line between the two sections is REQUIRED (do not omit it or replace it with >>>>>>> REPLACE):\n<<<<<<< SEARCH\n<exact lines to find>\n=======\n<replacement lines>\n>>>>>>> REPLACE\nThe SEARCH text must match the file byte-for-byte. To create a new file, leave the SEARCH section empty. The ``` fence is optional." + anchorNote()},
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

// DefaultRegistry builds the registry with the builtin tool vocabulary, all jailed
// to ws. opts configure run_command (timeout, output bound).
//
// When ws withholds the model-facing shell (ws.ModelShellDisabled — a scope policy
// is active, or patch-only mode is set), run_command is NOT advertised to the
// model: a hidden stand-in is registered instead, so a fallback-adapter run_command
// call is rejected before any process runs (disabled_shell.go). The file tools
// (edit_file/write_file) remain, still subject to the scope policy carried by ws.
func DefaultRegistry(ws Workspace, opts ...RunCommandOption) *Registry {
	r := NewRegistry()
	bg := NewBackgroundManager()
	r.bg = bg
	r.Register(readFileTool{ws})
	r.Register(readDirTool{ws})
	r.Register(searchTool{ws})
	// KLOO_SIMPLE_EDIT=1 offers grok's three-field edit tool INSTEAD of the fenced
	// SEARCH/REPLACE one. Off by default, so the default tool vocabulary is
	// unchanged. Instead of, not alongside: two edit tools would confound the
	// measurement, since the question is whether the FORMAT suppresses editing.
	if simpleEdit() {
		r.Register(searchReplaceTool{ws})
	} else {
		r.Register(editFileTool{ws})
	}
	r.Register(writeFileTool{ws})
	r.Register(listDirTool{ws})
	if ws.ModelShellDisabled() {
		// Hidden: dispatchable (so a stray run_command is rejected pre-exec) but not
		// offered to the model. scope selects the failure shape (ScopeError vs patch-only).
		r.registerHidden(disabledRunCommand{scope: ws.ScopeActive()})
	} else {
		r.Register(NewRunCommandTool(ws, append(opts, WithBackground(bg))...))
	}
	r.Register(commandOutputTool{bg}) // read/stop background commands
	r.Register(finishTool{})          // explicit terminator; the loop intercepts it
	return r
}

// simpleEdit swaps kloo's fenced SEARCH/REPLACE edit tool for grok's three-field
// one (KLOO_SIMPLE_EDIT=1). Off by default.
func simpleEdit() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KLOO_SIMPLE_EDIT"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// searchReplaceTool is an edit tool with grok's ergonomics: three plain string
// fields instead of one string carrying fenced marker lines.
//
// Captured from grok through a logging proxy, running the SAME glimmer model on
// the same bench cases, its edit tool is:
//
//	{file_path, old_string, new_string, replace_all}
//
// kloo's edit_file instead asks the model to emit, inside a JSON string, a block
// with three marker lines in exact order (<<<<<<< SEARCH / ======= / >>>>>>>
// REPLACE). kloo's own schema text is long and defensive about that format, which
// is a tell.
//
// The measured problem this targets: kloo's glimmer reads 12-44 files and never
// edits on roughly half of runs, while grok's glimmer passes the same five cases
// 5/5. Only 4% of kloo runs record a FAILED edit, so the format is not breaking
// attempts — the hypothesis is that it suppresses them.
type searchReplaceTool struct{ ws Workspace }

func (t searchReplaceTool) Name() string { return "search_replace" }
func (t searchReplaceTool) Description() string {
	return "Replace an exact string in a file. old_string must match the file exactly and appear exactly once; add surrounding lines to make it unique. To create a new file, set old_string to an empty string." + anchorNote()
}

// anchorNote warns that read_file's "N\u2192" anchors are not part of the file, so an
// edit must match only what follows the arrow. Without it the anchors would turn
// every copied-out excerpt into a guaranteed no-match. grok's own search_replace
// description carries the same warning.
func anchorNote() string {
	if !lineAnchors() {
		return ""
	}
	return " read_file prefixes the first line and every tenth line with \"LINE_NUMBER\u2192\". That prefix is NOT part of the file: match only what comes after the \u2192, with its exact indentation."
}
func (t searchReplaceTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{
			"file_path":  {Type: "string", Description: "Workspace-relative path to the file to modify."},
			"old_string": {Type: "string", Description: "The exact text to replace. It must appear exactly once; add surrounding lines to make it unique."},
			"new_string": {Type: "string", Description: "The text to replace it with (must differ from old_string)."},
		},
		Required: []string{"file_path", "old_string", "new_string"},
	}
}
func (t searchReplaceTool) Invoke(ctx context.Context, c Call) (Result, error) {
	path, _ := argString(c.Args, "file_path")
	oldS, _ := argString(c.Args, "old_string")
	newS, _ := argString(c.Args, "new_string")
	// No replace_all: grok's schema has one, but kloo's SEARCH/REPLACE engine
	// replaces a single unique match and adding a flag the engine ignores would be
	// a schema that lies to the model — the exact failure class this tool exists to
	// remove.
	// Reuse the engine so scope policy, clobber guards and edit accounting behave
	// exactly as they do for edit_file.
	marker := "<<<<<<< SEARCH\n" + oldS + "\n=======\n" + newS + "\n>>>>>>> REPLACE"
	if err := EditFile(t.ws, path, path+"\n"+marker); err != nil {
		return Result{}, err
	}
	return Result{Output: "edited " + path}, nil
}
