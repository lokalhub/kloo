package repomap

import (
	"errors"
	"testing"
)

// forceCtags overrides the detection/run hooks for a test and restores them.
func forceCtags(t *testing.T, look func() (string, bool), run func(bin, root string, relpaths []string) ([]byte, error)) {
	t.Helper()
	origLook, origRun := ctagsLookPath, runCtagsCmd
	ctagsLookPath, runCtagsCmd = look, run
	t.Cleanup(func() { ctagsLookPath, runCtagsCmd = origLook, origRun })
}

// TestExtractRegexFallbackWhenCtagsAbsent is the current-host reality: ctags
// absent → Extract returns the regex extractor's symbols.
func TestExtractRegexFallbackWhenCtagsAbsent(t *testing.T) {
	forceCtags(t, func() (string, bool) { return "", false }, nil)

	root := goFixture(t)
	got := Extract(root, []string{"main.go", "internal/util/util.go"})
	if findSym(got, "Greet", KindFunc) == nil {
		t.Errorf("expected regex-extracted Greet, got %+v", got)
	}
	if findSym(got, "Config", KindType) == nil {
		t.Errorf("expected regex-extracted Config, got %+v", got)
	}
	// Should equal a direct regex extraction of the same files.
	want := extractViaRegex(root, []string{"main.go", "internal/util/util.go"})
	if len(got) != len(want) {
		t.Errorf("Extract (fallback) and regex disagree: %d vs %d symbols", len(got), len(want))
	}
}

// TestExtractCtagsErrorDegradesToRegex: ctags present but the invocation errors
// → Extract still returns regex symbols, no crash.
func TestExtractCtagsErrorDegradesToRegex(t *testing.T) {
	forceCtags(t,
		func() (string, bool) { return "ctags", true },
		func(bin, root string, relpaths []string) ([]byte, error) { return nil, errors.New("boom") },
	)
	got := Extract(goFixture(t), []string{"main.go"})
	if findSym(got, "Greet", KindFunc) == nil {
		t.Errorf("ctags error should degrade to regex; got %+v", got)
	}
}

// TestExtractUsesCtagsWhenPresent: ctags present and returning JSON → Extract
// uses the ctags-derived symbols (proven by a symbol the regex extractor would
// not produce for this file).
func TestExtractUsesCtagsWhenPresent(t *testing.T) {
	transcript := `{"_type":"ptag","name":"!_TAG_FILE_FORMAT"}
{"_type":"tag","name":"CtagsOnlySym","path":"main.go","line":42,"kind":"function"}
{"_type":"tag","name":"WidgetClass","path":"app.ts","line":7,"kind":"class"}`
	forceCtags(t,
		func() (string, bool) { return "ctags", true },
		func(bin, root string, relpaths []string) ([]byte, error) { return []byte(transcript), nil },
	)
	got := Extract(goFixture(t), []string{"main.go"})
	if s := findSym(got, "CtagsOnlySym", KindFunc); s == nil || s.Line != 42 {
		t.Errorf("expected ctags-derived CtagsOnlySym@42, got %+v", got)
	}
	// And it did NOT fall back to regex (regex's Greet must be absent).
	if findSym(got, "Greet", KindFunc) != nil {
		t.Errorf("ctags path should not also include regex symbols")
	}
}

// TestParseCtagsJSONMapsKinds: the JSON parser maps ctags kinds onto the uniform
// vocabulary and skips ptag/junk lines.
func TestParseCtagsJSONMapsKinds(t *testing.T) {
	raw := `{"_type":"ptag","name":"!_TAG_PROGRAM_NAME","pattern":"Universal Ctags"}
{"_type":"tag","name":"Greet","path":"main.go","line":10,"kind":"function"}
{"_type":"tag","name":"Config","path":"util.go","line":3,"kind":"struct"}
{"_type":"tag","name":"DefaultName","path":"util.go","line":7,"kind":"constant"}
{"_type":"tag","name":"HomePage","path":"home.ts","line":7,"kind":"class"}
not-json-junk-line
{"_type":"tag","name":"","path":"x","line":1,"kind":"function"}`
	syms := parseCtagsJSON([]byte(raw))

	if s := findSym(syms, "Greet", KindFunc); s == nil || s.File != "main.go" || s.Line != 10 {
		t.Errorf("Greet mapping wrong: %+v", syms)
	}
	if findSym(syms, "Config", KindType) == nil {
		t.Errorf("struct should map to type: %+v", syms)
	}
	if findSym(syms, "DefaultName", KindConst) == nil {
		t.Errorf("constant should map to const: %+v", syms)
	}
	if findSym(syms, "HomePage", KindClass) == nil {
		t.Errorf("class should map to class: %+v", syms)
	}
	// ptag, junk, and empty-name lines are skipped → exactly 4 symbols.
	if len(syms) != 4 {
		t.Errorf("want 4 parsed symbols, got %d: %+v", len(syms), syms)
	}
}

// TestCtagsAndRegexSameShape: both backends populate the same Symbol fields.
func TestCtagsAndRegexSameShape(t *testing.T) {
	regexSyms := extractViaRegex(goFixture(t), []string{"main.go"})
	ctagsSyms := parseCtagsJSON([]byte(`{"_type":"tag","name":"Greet","path":"main.go","line":10,"kind":"function"}`))
	for _, set := range [][]Symbol{regexSyms, ctagsSyms} {
		for _, s := range set {
			if s.Name == "" || s.Kind == "" || s.File == "" || s.Line <= 0 {
				t.Errorf("symbol shape incomplete: %+v", s)
			}
		}
	}
}
