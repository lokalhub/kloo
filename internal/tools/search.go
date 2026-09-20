package tools

import (
	"context"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/lokalhub/kloo/internal/repomap"
)

// NameSearch is the search tool name.
const NameSearch = "search"

const (
	searchMaxMatches = 200       // cap matches per call so output can't flood the window
	searchMaxOutput  = 64 * 1024 // 64 KiB total
	searchMaxLineLen = 400       // truncate a long matched line (e.g. a minified bundle)

	// KLOO_TIGHT_SEARCH bounds. 64 KiB is ~16k tokens — roughly a THIRD of the hot
	// budget at ctx 131072 — spent on one navigation call. Measured on kloo-bench
	// C66: consecutive `search` turns grew the conversation ~19k tokens each, which
	// is what drives the run into compaction, sheds the file it just read, and
	// produces the re-read loop. grok's captured `grep` bounds its output by a
	// head_limit instead and its runs are ~5x shorter.
	tightSearchMaxMatches = 60
	tightSearchMaxOutput  = 8 * 1024
	tightSearchMaxLineLen = 240
)

// searchBounds returns the per-call output bounds for this run.
func searchBounds() (maxMatches, maxOutput, maxLineLen int) {
	if envOn("KLOO_TIGHT_SEARCH") {
		return tightSearchMaxMatches, tightSearchMaxOutput, tightSearchMaxLineLen
	}
	return searchMaxMatches, searchMaxOutput, searchMaxLineLen
}

// searchTool is the search tool: it scans the workspace for a regular expression
// and returns bounded `file:line: matched line` results. It reuses repomap.Walk, so
// it skips the same dependency/build dirs the repo map does and honours .gitignore;
// binary/oversize files are skipped. This is the find→read→edit backbone for code
// navigation, far more reliable than the model hand-rolling `grep` via run_command.
type searchTool struct{ ws Workspace }

func (t searchTool) Name() string { return NameSearch }
func (t searchTool) Description() string {
	return "Search the workspace for a regular expression and return matching file:line: lines, so you can find where something is defined or used. Skips dependency/build dirs and .gitignored files. The query is a regex (use (?i) at the start for case-insensitive). Output is bounded; narrow the query or path if it reports truncation."
}
func (t searchTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{
			"query":      {Type: "string", Description: "Regular expression to search for (use (?i) prefix for case-insensitive)."},
			"path":       {Type: "string", Description: "Optional workspace-relative folder or file to limit the search to (default: the whole workspace)."},
			"head_limit": {Type: "integer", Description: "Optional maximum number of matching lines to return, like piping to head -N."},
		},
		Required: []string{"query"},
	}
}

func (t searchTool) Invoke(ctx context.Context, c Call) (Result, error) {
	query, _ := argString(c.Args, "query")
	if strings.TrimSpace(query) == "" {
		return Result{}, fmt.Errorf("tools: search: empty query")
	}
	re, err := regexp.Compile(query)
	if err != nil {
		return Result{}, fmt.Errorf("tools: search: invalid regex %q: %w", query, err)
	}

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
		return Result{}, fmt.Errorf("tools: search %s: %w", rel, err)
	}

	// Build the candidate file list: a single file, or the skip-aware walk of a dir.
	type cand struct{ relToWs string }
	var cands []cand
	if info.IsDir() {
		nodes, werr := repomap.Walk(abs)
		if werr != nil {
			return Result{}, fmt.Errorf("tools: search %s: %w", rel, werr)
		}
		files := repomap.Files(nodes)
		sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
		for _, f := range files {
			r := f.Path
			if rel != "." {
				r = path.Join(rel, f.Path)
			}
			cands = append(cands, cand{r})
		}
	} else {
		cands = []cand{{rel}}
	}

	var body strings.Builder
	maxMatches, maxOutput, maxLineLen := searchBounds()
	// head_limit caps matches for this call only (grok's grep has the same knob);
	// it can tighten the default but never raise it past the bound above.
	if hl := argInt(c.Args, "head_limit"); hl > 0 && hl < maxMatches {
		maxMatches = hl
	}
	matches, filesHit, truncated := 0, 0, false
	for _, cd := range cands {
		content, rerr := ReadFile(t.ws, cd.relToWs)
		if rerr != nil || strings.IndexByte(content, 0) >= 0 { // unreadable/oversize or binary
			continue
		}
		fileHadMatch := false
		for i, line := range strings.Split(content, "\n") {
			if !re.MatchString(line) {
				continue
			}
			shown := strings.TrimRight(line, "\r")
			if len(shown) > maxLineLen {
				shown = shown[:maxLineLen] + "…"
			}
			fmt.Fprintf(&body, "%s:%d: %s\n", cd.relToWs, i+1, strings.TrimSpace(shown))
			matches++
			fileHadMatch = true
			if matches >= maxMatches || body.Len() >= maxOutput {
				truncated = true
				break
			}
		}
		if fileHadMatch {
			filesHit++
		}
		if truncated {
			break
		}
	}

	var head strings.Builder
	if matches == 0 {
		fmt.Fprintf(&head, "no matches for /%s/", query)
		if rel != "." {
			fmt.Fprintf(&head, " in %s", rel)
		}
		return Result{Output: head.String() + "\n"}, nil
	}
	fmt.Fprintf(&head, "found %d match(es) for /%s/ in %d file(s)", matches, query, filesHit)
	if truncated {
		head.WriteString(" — TRUNCATED (hit the limit); narrow the query or path")
	}
	head.WriteString(":\n\n")
	return Result{Output: head.String() + body.String()}, nil
}
