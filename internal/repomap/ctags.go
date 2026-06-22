package repomap

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Extract returns the symbols for the given workspace-relative files, rooted at
// root. It uses universal-ctags as an accelerator IF it is on PATH, and falls
// back TRANSPARENTLY to the pure-Go regex extractor (symbols.go) otherwise — the
// caller gets the same Symbol shape either way and never sees an error from a
// missing or misbehaving ctags. ctags is invoked as an external PROCESS (never
// linked): there is no CGO here.
//
// On this host ctags is absent (pre-flight #4), so the regex path is what runs;
// the ctags branch is exercised via the parser unit test + the overridable
// hooks below.
func Extract(root string, relpaths []string) []Symbol {
	if bin, ok := ctagsLookPath(); ok {
		if syms, err := extractViaCtags(bin, root, relpaths); err == nil {
			return syms
		}
		// ctags present but failed/emitted junk → degrade to regex, no crash.
	}
	return extractViaRegex(root, relpaths)
}

// Overridable hooks so tests can simulate ctags present/absent/failing without
// requiring the binary on the host.
var (
	ctagsLookPath = defaultCtagsLookPath
	runCtagsCmd   = defaultRunCtags
)

// defaultCtagsLookPath reports whether a Universal Ctags binary is on PATH.
func defaultCtagsLookPath() (string, bool) {
	p, err := exec.LookPath("ctags")
	if err != nil {
		return "", false
	}
	out, err := exec.Command(p, "--version").Output()
	if err != nil || !strings.Contains(string(out), "Universal Ctags") {
		return "", false // a non-universal ctags (e.g. exuberant/BSD) is not used
	}
	return p, true
}

// defaultRunCtags runs ctags with stable JSON output over the given files.
func defaultRunCtags(bin, root string, relpaths []string) ([]byte, error) {
	args := append([]string{"--output-format=json", "-f", "-"}, relpaths...)
	cmd := exec.Command(bin, args...)
	cmd.Dir = root
	return cmd.Output()
}

func extractViaCtags(bin, root string, relpaths []string) ([]Symbol, error) {
	raw, err := runCtagsCmd(bin, root, relpaths)
	if err != nil {
		return nil, err
	}
	return parseCtagsJSON(raw), nil
}

func extractViaRegex(root string, relpaths []string) []Symbol {
	var out []Symbol
	for _, rel := range relpaths {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue // unreadable file: skip, don't crash the whole extract
		}
		out = append(out, ExtractSymbols(rel, content)...)
	}
	return out
}

// ctagsTag is one line of `ctags --output-format=json` output.
type ctagsTag struct {
	Type string `json:"_type"`
	Name string `json:"name"`
	Path string `json:"path"`
	Line int    `json:"line"`
	Kind string `json:"kind"`
}

// parseCtagsJSON parses newline-delimited ctags JSON into Symbols, mapping
// ctags' kind vocabulary onto the uniform kinds the regex extractor emits.
func parseCtagsJSON(raw []byte) []Symbol {
	var syms []Symbol
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var tag ctagsTag
		if err := json.Unmarshal([]byte(line), &tag); err != nil {
			continue // skip junk/ptag header lines, don't fail the whole parse
		}
		if tag.Type != "tag" || tag.Name == "" {
			continue
		}
		syms = append(syms, Symbol{
			Name: tag.Name,
			Kind: mapCtagsKind(tag.Kind),
			File: filepath.ToSlash(tag.Path),
			Line: tag.Line,
		})
	}
	return syms
}

// mapCtagsKind normalises ctags kinds to the uniform Symbol kinds.
func mapCtagsKind(k string) string {
	switch k {
	case "function", "func":
		return KindFunc
	case "method":
		return KindMethod
	case "class":
		return KindClass
	case "struct", "interface", "typedef", "type", "enum":
		return KindType
	case "variable", "var":
		return KindVar
	case "constant", "const", "macro":
		return KindConst
	default:
		return k // preserve any other kind rather than dropping the symbol
	}
}
