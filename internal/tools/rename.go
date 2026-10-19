package tools

import (
	"context"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"

	"github.com/lokalhub/kloo/internal/repomap"
)

// NameRenameSymbol is the rename_symbol tool name.
const NameRenameSymbol = "rename_symbol"

// renameMaxFiles caps how many files one call will report on, so a rename of a very
// common identifier across a huge tree cannot flood the window the way a raw
// workspace-wide grep would. The truncation is always reported, never silent.
const renameMaxFiles = 200

// RenameSymbolIn replaces every IDENTIFIER-BOUNDED occurrence of old with new in
// content and reports how many it replaced. Bounded means the character before and
// after the match is not a letter, digit or underscore, so renaming `measured` touches
// `.measured()`, `SeatCaps::measured()` and `fn measured(` but leaves `measured_run`
// and `remeasured` alone. It is a pure function so the boundary rule can be tested
// without a workspace, a model or a filesystem.
//
// The boundary rule is the whole tool: a plain string replace would turn
// `measured_run` into `default_caps_run` and silently break the build, which is the
// failure class this primitive exists to remove. It is deliberately NOT
// language-aware — it does not parse, so it also matches the same word used as
// ENGLISH inside a comment ("measured against this host's table"). That hazard is
// why the tool reports before it writes (see renameSymbolTool.Invoke), not why it
// would refuse.
func RenameSymbolIn(content, old, new string) (string, int) {
	if old == "" {
		return content, 0
	}
	var b strings.Builder
	n, i := 0, 0
	for {
		j := strings.Index(content[i:], old)
		if j < 0 {
			break
		}
		j += i
		prev, next := byte(' '), byte(' ')
		if j > 0 {
			prev = content[j-1]
		}
		if j+len(old) < len(content) {
			next = content[j+len(old)]
		}
		if isIdentByte(prev) || isIdentByte(next) {
			// Not a whole identifier: part of a longer token (measured_run,
			// remeasured). Copy it through untouched and keep scanning after it.
			b.WriteString(content[i : j+len(old)])
			i = j + len(old)
			continue
		}
		b.WriteString(content[i:j])
		b.WriteString(new)
		n++
		i = j + len(old)
	}
	b.WriteString(content[i:])
	return b.String(), n
}

// isIdentByte reports whether c can sit inside an identifier, so a match flanked by
// one of these is a fragment of a longer token rather than the symbol itself.
func isIdentByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') ||
		(c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

// isIdentifier reports whether s is a single [A-Za-z_][A-Za-z0-9_]* token.
//
// Both ends of a rename are checked with this before anything is read or written. A
// small model asks for `old: "measured table"` (a phrase, which would match nothing
// and read as a silent no-op) or `new: "2bad"` (not a legal identifier, which would
// write a file that does not compile). Refusing names the offending argument instead.
func isIdentifier(s string) bool {
	if s == "" {
		return false
	}
	if !(s[0] == '_' || (s[0] >= 'a' && s[0] <= 'z') || (s[0] >= 'A' && s[0] <= 'Z')) {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isIdentByte(s[i]) {
			return false
		}
	}
	return true
}

// renameSymbolTool renames an identifier across the workspace in ONE call.
//
// Why it exists: three runs churned on a purely mechanical rename (`SeatCaps::
// measured()` → `default_caps()`) across four Rust files. Two of them spent every
// step surveying — search, read, search again — and made zero edits, because the
// available path was N searches plus N hand-written SEARCH/REPLACE blocks, and the
// explore rail has nothing to offer a model that is correctly looking for the next
// place the name occurs. This makes that task one call, and it reports the file list
// first so the caller can see what it is about to touch.
type renameSymbolTool struct{ ws Workspace }

func (t renameSymbolTool) Name() string { return NameRenameSymbol }
func (t renameSymbolTool) Description() string {
	return "Rename an identifier across the workspace in one call: every occurrence of old whose surrounding characters are not a letter, digit or underscore becomes new, so .measured(), SeatCaps::measured() and fn measured( all change while measured_run and remeasured are left alone. It does not parse code, so it also matches the same word used as English in a comment. WITHOUT confirm it only REPORTS the files and per-file counts and writes NOTHING: call it once to see the list, then again with confirm=true to apply it. Use this instead of searching and hand-editing each file. With literal=true, old and new may be any text rather than one identifier, so a phrase such as \"measured table\" can be renamed to \"default table\" in the same single call (old must still be non-empty; new may be empty to delete the phrase)."
}
func (t renameSymbolTool) Schema() ParamSchema {
	return ParamSchema{
		Properties: map[string]Property{
			"old":     {Type: "string", Description: "The identifier to rename (one [A-Za-z_][A-Za-z0-9_]* token, e.g. measured)."},
			"new":     {Type: "string", Description: "The new identifier (one [A-Za-z_][A-Za-z0-9_]* token, e.g. default_caps)."},
			"path":    {Type: "string", Description: "Optional workspace-relative folder or single file to limit the rename to (default: the whole workspace)."},
			"confirm": {Type: "boolean", Description: "false/absent: report the files and counts, write NOTHING (always do this first). true: actually rewrite the files."},
			"literal": {Type: "boolean", Description: "true: old and new may be any text, not just one identifier (e.g. renaming the phrase \"measured table\" to \"default table\"). The boundary rule and the report-first rule are unchanged."},
		},
		Required: []string{"old", "new"},
	}
}

// renameHit is one file the rename would touch, or already collides in.
type renameHit struct {
	rel   string // workspace-relative path, as reported
	count int    // identifier-bounded occurrences of old
	coll  bool   // the NEW name already appears here as an identifier
}

func (t renameSymbolTool) Invoke(ctx context.Context, c Call) (Result, error) {
	oldName, _ := argString(c.Args, "old")
	newName, _ := argString(c.Args, "new")
	// literal is read BEFORE the identifier checks because it is what relaxes them.
	// It relaxes the SHAPE of the arguments and nothing else: the boundary rule, the
	// collision report, report-first, the walk and the binary skip are the same code
	// path either way, so there is no second implementation to drift.
	literal := argBool(c.Args, "literal")
	if !literal {
		if !isIdentifier(oldName) {
			return Result{}, fmt.Errorf("tools: rename_symbol: argument \"old\" must be a single identifier ([A-Za-z_][A-Za-z0-9_]*), got %q — rename_symbol renames one symbol, not a phrase; pass literal: true to rename a phrase", oldName)
		}
		if !isIdentifier(newName) {
			return Result{}, fmt.Errorf("tools: rename_symbol: argument \"new\" must be a single identifier ([A-Za-z_][A-Za-z0-9_]*), got %q — the replacement must be a valid identifier, or pass literal: true to replace a phrase", newName)
		}
	} else if oldName == "" {
		// An empty `old` matches everywhere, so literal mode must still refuse it.
		// An empty `new` is allowed: deleting a phrase is a legitimate rename.
		return Result{}, fmt.Errorf("tools: rename_symbol: argument \"old\" must not be empty, even with literal: true — an empty old matches at every position and would rewrite every file")
	}
	if oldName == newName {
		return Result{}, fmt.Errorf("tools: rename_symbol: \"old\" and \"new\" are both %q — there is nothing to rename", oldName)
	}

	rel := strings.TrimSpace(mustArgString(c.Args, "path"))
	if rel == "" {
		rel = "."
	}
	abs, err := t.ws.Resolve(rel)
	if err != nil {
		return Result{}, err // ErrPathEscape surfaces unchanged
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Result{}, fmt.Errorf("tools: rename_symbol %s: %w", rel, err)
	}

	// Candidate files: a single file, or the skip-aware walk of a folder — the same
	// list search uses, so dependency/build dirs and .gitignored paths are never
	// rewritten by a rename the caller never saw.
	type cand struct{ relToWs string }
	var cands []cand
	if info.IsDir() {
		nodes, werr := repomap.Walk(abs)
		if werr != nil {
			return Result{}, fmt.Errorf("tools: rename_symbol %s: %w", rel, werr)
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

	confirm := argBool(c.Args, "confirm")
	var hits, collides []renameHit
	total, scanned, truncated := 0, 0, false
	for _, cd := range cands {
		content, rerr := ReadFile(t.ws, cd.relToWs)
		if rerr != nil || strings.IndexByte(content, 0) >= 0 { // unreadable/oversize or binary
			continue
		}
		scanned++
		updated, n := RenameSymbolIn(content, oldName, newName)
		// Reusing the pure seam to detect a collision: replacing newName with
		// itself counts its whole-identifier occurrences, and >0 means the rename
		// would land on a name that is already used here.
		_, already := RenameSymbolIn(content, newName, newName)
		if n == 0 && already == 0 {
			continue
		}
		if len(hits) >= renameMaxFiles && n > 0 {
			truncated = true
			continue
		}
		if n > 0 {
			hits = append(hits, renameHit{rel: cd.relToWs, count: n, coll: already > 0})
			total += n
		} else {
			collides = append(collides, renameHit{rel: cd.relToWs, coll: true})
		}
		// A file that both holds the old name and already uses the new one belongs
		// in the collision list too — otherwise the heading below would silently
		// omit exactly the files where the rename is most likely to be wrong.
		if n > 0 && already > 0 {
			collides = append(collides, renameHit{rel: cd.relToWs, coll: true})
		}
		if confirm && n > 0 {
			if werr := WriteFile(t.ws, cd.relToWs, updated); werr != nil {
				return Result{}, fmt.Errorf("tools: rename_symbol %s: %w", cd.relToWs, werr)
			}
		}
	}

	var out strings.Builder
	if total == 0 {
		// Never an empty result: a rename that found nothing has to say where it
		// looked, or the caller re-runs it with a different path and churns.
		fmt.Fprintf(&out, "no occurrences of %s found as an identifier in %s (%d text file(s) scanned) — nothing to rename, nothing written", oldName, rel, scanned)
		if len(collides) > 0 {
			out.WriteString("\n\nthe new name already appears here:\n")
			for _, h := range collides {
				fmt.Fprintf(&out, "  %s\n", h.rel)
			}
		}
		out.WriteString("\n")
		return Result{Output: out.String()}, nil
	}

	verb := "would rename"
	if confirm {
		verb = "renamed"
	}
	fmt.Fprintf(&out, "%s %s → %s: %d occurrence(s) in %d file(s) (scanned %d text file(s) under %s)\n\n", verb, oldName, newName, total, len(hits), scanned, rel)
	for _, h := range hits {
		if confirm {
			fmt.Fprintf(&out, "  %s: %d occurrence(s) written\n", h.rel, h.count)
		} else {
			fmt.Fprintf(&out, "  %s: %d occurrence(s)\n", h.rel, h.count)
		}
	}
	if truncated {
		fmt.Fprintf(&out, "  … TRUNCATED at %d files; narrow path to rename the rest\n", renameMaxFiles)
	}

	// The collision report is advisory, never a refusal: renaming `caps` to
	// `measured` in a file that already has a `measured` may be exactly what the
	// caller means (a merge of two symbols), and it may not. The caller decides —
	// but it has to see it either way.
	if len(collides) > 0 {
		out.WriteString("\nWARNING — the rename would COLLIDE (the new name already appears as an identifier in these files):\n")
		for _, h := range collides {
			fmt.Fprintf(&out, "  %s\n", h.rel)
		}
	}

	if !confirm {
		out.WriteString("\nNOTHING WAS WRITTEN — this is a report only. Re-issue the same call with confirm: true to apply the rename to these files. Check the list first: an identifier-bounded rename also rewrites the same word used as English in a comment.")
		out.WriteString("\n")
		return Result{Output: out.String()}, nil
	}
	fmt.Fprintf(&out, "\nwrote %d file(s).\n", len(hits))
	return Result{Output: out.String()}, nil
}

// mustArgString returns a string argument or "" when absent/not-a-string, for
// optional arguments whose caller only ever trims the result.
func mustArgString(args map[string]any, key string) string {
	s, _ := argString(args, key)
	return s
}
