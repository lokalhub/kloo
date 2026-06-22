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

// hardSkipDirs are never descended into, regardless of .gitignore.
var hardSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"dist":         true,
	"bin":          true,
}

// Node is one entry of the walked tree: its workspace-relative path, whether it
// is a directory, and its size in bytes (0 for dirs) so later tasks can budget
// without re-statting.
type Node struct {
	Path  string // slash-separated, relative to the walk root
	IsDir bool
	Size  int64
}

// Walk enumerates the repo rooted at root into a deterministically ordered slice
// of Nodes (lexicographic by Path). It hard-skips .git/node_modules/dist/bin,
// honours the repo's .gitignore (pure-Go matching — never shells out to git),
// and does not follow symlinks (so it can never escape the workspace via a
// symlinked directory). root is expected to be the canonical workspace root
// (e.g. tools.Workspace.Root()).
func Walk(root string) ([]Node, error) {
	ig, err := loadGitignore(root)
	if err != nil {
		return nil, err
	}

	var nodes []Node
	err = walkDir(root, root, ig, &nodes)
	if err != nil {
		return nil, err
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Path < nodes[j].Path })
	return nodes, nil
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

func walkDir(root, dir string, ig *gitignore, out *[]Node) error {
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
			if ig.match(rel, true) {
				continue
			}
			*out = append(*out, Node{Path: rel, IsDir: true})
			if err := walkDir(root, abs, ig, out); err != nil {
				return err
			}
			continue
		}

		if ig.match(rel, false) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
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

// loadGitignore reads <root>/.gitignore if present (a missing file is fine).
func loadGitignore(root string) (*gitignore, error) {
	data, err := os.ReadFile(filepath.Join(root, ".gitignore"))
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
