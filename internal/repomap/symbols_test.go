package repomap

import (
	"os"
	"path/filepath"
	"testing"
)

func readFixture(t *testing.T, repo, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "repos", repo, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read fixture %s/%s: %v", repo, rel, err)
	}
	return b
}

// findSym returns the symbol with the given name+kind, or nil.
func findSym(syms []Symbol, name, kind string) *Symbol {
	for i := range syms {
		if syms[i].Name == name && syms[i].Kind == kind {
			return &syms[i]
		}
	}
	return nil
}

func TestExtractGoSymbols(t *testing.T) {
	syms := ExtractSymbols("main.go", readFixture(t, "go-fixture", "main.go"))
	if s := findSym(syms, "Greet", KindFunc); s == nil || s.Line != 10 {
		t.Errorf("Greet func not found at line 10: %+v", syms)
	}
	if s := findSym(syms, "main", KindFunc); s == nil || s.Line != 5 {
		t.Errorf("main func not found at line 5: %+v", syms)
	}

	usyms := ExtractSymbols("util.go", readFixture(t, "go-fixture", "internal/util/util.go"))
	if s := findSym(usyms, "Config", KindType); s == nil || s.Line != 3 {
		t.Errorf("Config type not found at line 3: %+v", usyms)
	}
	if findSym(usyms, "DefaultName", KindConst) == nil {
		t.Errorf("DefaultName const not extracted: %+v", usyms)
	}
	if findSym(usyms, "Normalize", KindFunc) == nil {
		t.Errorf("Normalize func not extracted: %+v", usyms)
	}
}

func TestExtractTSComponentAndSelector(t *testing.T) {
	syms := ExtractSymbols("profile.page.ts", readFixture(t, "ionic-fixture", "src/app/profile/profile.page.ts"))
	if s := findSym(syms, "ProfilePage", KindClass); s == nil || s.Line != 7 {
		t.Errorf("ProfilePage class not found at line 7: %+v", syms)
	}
	if s := findSym(syms, "app-profile", KindSelector); s == nil || s.Line != 4 {
		t.Errorf("app-profile selector not found at line 4: %+v", syms)
	}
}

func TestExtractHTMLSelectors(t *testing.T) {
	syms := ExtractSymbols("profile.page.html", readFixture(t, "ionic-fixture", "src/app/profile/profile.page.html"))
	for _, want := range []string{"ion-header", "ion-title", "app-profile-widget"} {
		if findSym(syms, want, KindSelector) == nil {
			t.Errorf("HTML selector %q not extracted: %+v", want, syms)
		}
	}
	// Closing tags must not be captured as selectors.
	for _, s := range syms {
		if s.Name == "/ion-header" {
			t.Errorf("closing tag captured as selector: %+v", s)
		}
	}
}

func TestExtractRoutePaths(t *testing.T) {
	syms := ExtractSymbols("app-routing.module.ts", readFixture(t, "ionic-fixture", "src/app/app-routing.module.ts"))
	for _, want := range []string{"home", "profile", "apps"} {
		if findSym(syms, want, KindRoute) == nil {
			t.Errorf("route path %q not extracted: %+v", want, syms)
		}
	}
	if findSym(syms, "AppRoutingModule", KindClass) == nil {
		t.Errorf("AppRoutingModule class not extracted: %+v", syms)
	}
}

func TestExtractNoSymbols(t *testing.T) {
	if got := ExtractSymbols("notes.txt", []byte("just prose, no code")); got != nil {
		t.Errorf("unknown extension should yield nil, got %+v", got)
	}
	if got := ExtractSymbols("empty.go", []byte("package empty\n")); len(got) != 0 {
		t.Errorf("symbol-free go file should yield no symbols, got %+v", got)
	}
}

func TestExtractUniformShape(t *testing.T) {
	all := [][]Symbol{
		ExtractSymbols("main.go", readFixture(t, "go-fixture", "main.go")),
		ExtractSymbols("profile.page.ts", readFixture(t, "ionic-fixture", "src/app/profile/profile.page.ts")),
		ExtractSymbols("profile.page.html", readFixture(t, "ionic-fixture", "src/app/profile/profile.page.html")),
	}
	for _, syms := range all {
		for _, s := range syms {
			if s.Name == "" || s.Kind == "" || s.File == "" || s.Line <= 0 {
				t.Errorf("symbol has an empty field: %+v", s)
			}
		}
	}
}
