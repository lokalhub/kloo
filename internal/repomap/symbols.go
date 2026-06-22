package repomap

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Symbol kinds — a uniform, backend-agnostic vocabulary so the regex extractor
// (this file) and the ctags backend (ctags.go) produce the same shape.
const (
	KindFunc     = "function"
	KindMethod   = "method"
	KindType     = "type"
	KindVar      = "var"
	KindConst    = "const"
	KindClass    = "class"
	KindSelector = "selector" // Angular component selector (e.g. app-home)
	KindRoute    = "route"    // route path (router config / routerLink)
)

// Symbol is one extracted top-level symbol. The shape is identical across
// languages and across the regex/ctags backends.
type Symbol struct {
	Name string
	Kind string
	File string // workspace-relative path
	Line int    // 1-based
}

// Line-oriented regexes. This is intentionally NOT a parser (no CGO,
// no tree-sitter) — good enough for ranking, with documented gaps (decisions.md).
var (
	reGoFunc   = regexp.MustCompile(`^func\s+(\w+)\s*\(`)
	reGoMethod = regexp.MustCompile(`^func\s+\([^)]*\)\s+(\w+)\s*\(`)
	reGoType   = regexp.MustCompile(`^type\s+(\w+)\b`)
	reGoVar    = regexp.MustCompile(`^var\s+(\w+)\b`)
	reGoConst  = regexp.MustCompile(`^const\s+(\w+)\b`)

	reTSClass    = regexp.MustCompile(`(?:^|\s)class\s+(\w+)`)
	reTSFunc     = regexp.MustCompile(`(?:^|\s)function\s+(\w+)`)
	reTSExport   = regexp.MustCompile(`^export\s+const\s+(\w+)`)
	reTSSelector = regexp.MustCompile(`selector\s*:\s*['"]([^'"]+)['"]`)
	reTSPath     = regexp.MustCompile(`\bpath\s*:\s*['"]([^'"]*)['"]`)

	reHTMLTag        = regexp.MustCompile(`<([a-z][a-z0-9]*-[a-z0-9-]+)`)
	reHTMLRouterLink = regexp.MustCompile(`routerLink\s*=\s*['"]([^'"]+)['"]`)
)

// ExtractSymbols returns the symbols in a file, dispatched by extension. An
// unknown extension or a file with no recognisable symbols returns nil (never a
// panic).
func ExtractSymbols(file string, content []byte) []Symbol {
	switch strings.ToLower(filepath.Ext(file)) {
	case ".go":
		return extractGo(file, content)
	case ".ts", ".tsx", ".js", ".jsx":
		return extractTS(file, content)
	case ".html":
		return extractHTML(file, content)
	default:
		return nil
	}
}

func extractGo(file string, content []byte) []Symbol {
	var syms []Symbol
	eachLine(content, func(line string, n int) {
		switch {
		case reGoMethod.MatchString(line):
			syms = add(syms, reGoMethod, line, KindMethod, file, n)
		case reGoFunc.MatchString(line):
			syms = add(syms, reGoFunc, line, KindFunc, file, n)
		case reGoType.MatchString(line):
			syms = add(syms, reGoType, line, KindType, file, n)
		case reGoConst.MatchString(line):
			syms = add(syms, reGoConst, line, KindConst, file, n)
		case reGoVar.MatchString(line):
			syms = add(syms, reGoVar, line, KindVar, file, n)
		}
	})
	return syms
}

func extractTS(file string, content []byte) []Symbol {
	var syms []Symbol
	eachLine(content, func(line string, n int) {
		if m := reTSClass.FindStringSubmatch(line); m != nil {
			syms = append(syms, Symbol{Name: m[1], Kind: KindClass, File: file, Line: n})
		}
		if m := reTSFunc.FindStringSubmatch(line); m != nil {
			syms = append(syms, Symbol{Name: m[1], Kind: KindFunc, File: file, Line: n})
		}
		if m := reTSExport.FindStringSubmatch(line); m != nil {
			syms = append(syms, Symbol{Name: m[1], Kind: KindConst, File: file, Line: n})
		}
		if m := reTSSelector.FindStringSubmatch(line); m != nil {
			syms = append(syms, Symbol{Name: m[1], Kind: KindSelector, File: file, Line: n})
		}
		if m := reTSPath.FindStringSubmatch(line); m != nil && m[1] != "" {
			syms = append(syms, Symbol{Name: m[1], Kind: KindRoute, File: file, Line: n})
		}
	})
	return syms
}

func extractHTML(file string, content []byte) []Symbol {
	var syms []Symbol
	seen := map[string]bool{}
	eachLine(content, func(line string, n int) {
		for _, m := range reHTMLTag.FindAllStringSubmatch(line, -1) {
			key := KindSelector + ":" + m[1]
			if !seen[key] {
				seen[key] = true
				syms = append(syms, Symbol{Name: m[1], Kind: KindSelector, File: file, Line: n})
			}
		}
		if m := reHTMLRouterLink.FindStringSubmatch(line); m != nil {
			syms = append(syms, Symbol{Name: m[1], Kind: KindRoute, File: file, Line: n})
		}
	})
	return syms
}

// add appends the first capture group of re applied to line as a symbol.
func add(syms []Symbol, re *regexp.Regexp, line, kind, file string, n int) []Symbol {
	m := re.FindStringSubmatch(line)
	if m == nil {
		return syms
	}
	return append(syms, Symbol{Name: m[1], Kind: kind, File: file, Line: n})
}

// eachLine invokes fn for each line of content with a 1-based line number.
func eachLine(content []byte, fn func(line string, n int)) {
	for i, line := range strings.Split(string(content), "\n") {
		fn(strings.TrimRight(line, "\r"), i+1)
	}
}
