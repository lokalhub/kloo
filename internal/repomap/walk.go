// Package repomap builds the context repo map: it walks the workspace into a
// stable file tree (walk.go), extracts symbols with a pure-Go regex extractor —
// no CGO — accelerated by universal-ctags when present (symbols.go, ctags.go),
// ranks files/symbols by relevance to the task (rank.go), and curates a per-step
// context window under a token budget (budget.go).
//
// The no-CGO rule is central: there is no `import "C"` anywhere in this package
// and no dependency that needs CGO (tree-sitter is intentionally out of v1).
package repomap

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// hardSkipDirs are never descended into, regardless of .gitignore. These are
// VCS metadata, dependency trees, and build/cache output — none contain
// hand-written source the repo map should rank, and a single build can drop
// thousands of generated files (e.g. an Ionic build writes ~1.4k files into www/
// and ~400 into .angular/) that would otherwise flood the map and balloon the
// prompt every turn. They stay reachable via read_file/list_dir. Gitignored build
// output is also skipped (nested .gitignore, below); this list is the backstop for
// a project whose .gitignore doesn't cover it — or, as in the Ionic case, lives in
// a SUBDIR whose .gitignore the root walk wouldn't otherwise reach.
var hardSkipDirs = map[string]bool{
	// VCS + dependencies.
	".git": true, "node_modules": true, "vendor": true,
	"__pycache__": true, ".venv": true, "venv": true,
	// Generic build output.
	"dist": true, "build": true, "out": true, "bin": true, "target": true, "coverage": true,
	// Framework build output / caches.
	"www": true, ".angular": true, ".next": true, ".nuxt": true, ".svelte-kit": true,
	".output": true, ".cache": true, ".turbo": true, ".parcel-cache": true, ".vite": true,
	".gradle": true, ".pytest_cache": true, ".mypy_cache": true,
}

// maxMappedFileBytes bounds the size of a file the repo map will consider. Source
// files are small; anything larger (a checked-in binary, a model weight, a minified
// bundle, a giant lockfile/dataset) has no useful symbols AND is read WHOLE into
// memory by the extractor (ctags.go). A single multi-GB file under the walk root
// (e.g. a .gguf) would otherwise balloon the process and get it OOM-killed. Such
// files are simply skipped from the map; the model can still read_file/list_dir
// them directly. 1 MiB is generous for real source.
const maxMappedFileBytes = 1 << 20 // 1 MiB

// Node is one entry of the walked tree: its workspace-relative path, whether it
// is a directory, and its size in bytes (0 for dirs) so later tasks can budget
// without re-statting.
type Node struct {
	Path  string // slash-separated, relative to the walk root
	IsDir bool
	Size  int64
}

// Walk enumerates the repo rooted at root into a deterministically ordered slice
// of Nodes (lexicographic by Path). It hard-skips the dirs in hardSkipDirs,
// honours .gitignore at EVERY level (the root's and any nested one, pure-Go
// matching — never shells out to git), and does not follow symlinks (so it can
// never escape the workspace via a symlinked directory). root is expected to be
// the canonical workspace root (e.g. tools.Workspace.Root()).
func Walk(root string) ([]Node, error) {
	var nodes []Node
	if err := walkDir(root, root, nil, &nodes); err != nil {
		return nil, err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Path < nodes[j].Path })
	return nodes, nil
}

// scopedIgnore is one .gitignore and the directory its patterns are relative to.
// Like git, a .gitignore applies to its own directory subtree, matched against the
// path RELATIVE to that directory — so a project in a subdir (e.g. myTabsApp/)
// gets its own /www, /.angular ignores honoured even when the walk root is a
// parent with no .gitignore of its own.
type scopedIgnore struct {
	base string // absolute dir the patterns are relative to
	ig   *gitignore
}

// ignored reports whether abs is excluded by any .gitignore in scope.
func ignored(stack []scopedIgnore, abs string, isDir bool) bool {
	for _, s := range stack {
		if s.ig.match(relSlash(s.base, abs), isDir) {
			return true
		}
	}
	return false
}

// Files returns just the file Nodes from a walk result (preserving order).
func Files(nodes []Node) []Node {
	out := make([]Node, 0, len(nodes))
	for _, n := range nodes {
		if !n.IsDir {
			out = append(out, n)
		}
	}
	return out
}

func walkDir(root, dir string, stack []scopedIgnore, out *[]Node) error {
	// Push this directory's .gitignore (if any) onto the stack — its patterns apply
	// to everything below here. The full-slice copy (cap == len) means appending
	// can't clobber a sibling subtree's view of the stack.
	if ig, err := loadGitignore(dir); err != nil {
		return err
	} else if len(ig.patterns) > 0 {
		stack = append(stack[:len(stack):len(stack)], scopedIgnore{base: dir, ig: ig})
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		abs := filepath.Join(dir, name)
		rel := relSlash(root, abs)

		// Never follow symlinks — neither descend into a symlinked dir nor
		// include a symlinked file (keeps the walk inside the workspace).
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}

		if e.IsDir() {
			if hardSkipDirs[name] {
				continue
			}
			if ignored(stack, abs, true) {
				continue
			}
			*out = append(*out, Node{Path: rel, IsDir: true})
			if err := walkDir(root, abs, stack, out); err != nil {
				return err
			}
			continue
		}

		if ignored(stack, abs, false) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		// Skip files too large to map: they have no useful symbols and would be read
		// whole into memory downstream (a multi-GB file OOMs the process). They stay
		// reachable via the read_file/list_dir tools — just out of the repo map.
		if info.Size() > maxMappedFileBytes {
			continue
		}
		*out = append(*out, Node{Path: rel, IsDir: false, Size: info.Size()})
	}
	return nil
}

// relSlash returns abs relative to root, using forward slashes.
func relSlash(root, abs string) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		rel = abs
	}
	return filepath.ToSlash(rel)
}

// gitignore is a minimal pure-Go .gitignore matcher.
//
// Supported (the cases the Ionic fixture + kloo need): `*.ext` globs, plain dir
// or file names (matched at any depth by basename), leading `/` anchoring to the
// root, trailing `/` dir-only patterns, and `#` comments / blank lines.
//
// Deliberately NOT supported (decisions.md): `**` deep globs and `!` negation —
// patterns using them are ignored rather than half-honoured.
type gitignore struct {
	patterns []ignorePattern
}

type ignorePattern struct {
	glob     string // the cleaned pattern (no leading/trailing slash markers)
	anchored bool   // had a leading '/', or contains a '/': match against full rel path
	dirOnly  bool   // had a trailing '/': matches directories only
}

// loadGitignore reads <dir>/.gitignore if present (a missing file is fine). It is
// called once per directory during the walk so nested .gitignores are honoured.
func loadGitignore(dir string) (*gitignore, error) {
	data, err := os.ReadFile(filepath.Join(dir, ".gitignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return &gitignore{}, nil
		}
		return nil, err
	}
	return parseGitignore(string(data)), nil
}

func parseGitignore(text string) *gitignore {
	ig := &gitignore{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "!") || strings.Contains(line, "**") {
			continue // unsupported features: skip rather than half-apply
		}
		p := ignorePattern{}
		if strings.HasSuffix(line, "/") {
			p.dirOnly = true
			line = strings.TrimSuffix(line, "/")
		}
		if strings.HasPrefix(line, "/") {
			p.anchored = true
			line = strings.TrimPrefix(line, "/")
		} else if strings.Contains(line, "/") {
			p.anchored = true
		}
		p.glob = line
		if p.glob != "" {
			ig.patterns = append(ig.patterns, p)
		}
	}
	return ig
}

// match reports whether the relative path (slash-separated) is ignored.
func (ig *gitignore) match(rel string, isDir bool) bool {
	for _, p := range ig.patterns {
		if p.dirOnly && !isDir {
			continue
		}
		if p.anchored {
			if ok, _ := filepath.Match(p.glob, rel); ok {
				return true
			}
			// An anchored dir pattern also ignores everything beneath it.
			if strings.HasPrefix(rel, p.glob+"/") {
				return true
			}
			continue
		}
		// Unanchored: match against the basename at any depth.
		if ok, _ := filepath.Match(p.glob, pathBase(rel)); ok {
			return true
		}
		// Or a directory component named exactly the pattern (e.g. "build").
		for _, seg := range strings.Split(rel, "/") {
			if seg == p.glob {
				return true
			}
		}
	}
	return false
}

func pathBase(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[i+1:]
	}
	return rel
}
