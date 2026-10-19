package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenameSymbolInBoundaryRule: the boundary rule IS the tool. Renaming `measured`
// must reach every whole-identifier occurrence — after a dot, after `::`, after `fn `
// — and must leave the same letters inside a LONGER token byte-identical. A plain
// string replace would produce `default_caps_run` and `redefault_caps`, which
// compiles as nothing and breaks the build silently.
func TestRenameSymbolInBoundaryRule(t *testing.T) {
	const src = "let a = caps.measured();\n" +
		"let b = SeatCaps::measured();\n" +
		"fn measured() -> Caps {}\n" +
		"let c = measured_run(1);\n" +
		"let d = remeasured;\n"
	got, n := RenameSymbolIn(src, "measured", "default_caps")
	if n != 3 {
		t.Errorf("replacements = %d, want 3\n---\n%s", n, got)
	}
	for _, want := range []string{
		"caps.default_caps()",
		"SeatCaps::default_caps()",
		"fn default_caps() -> Caps",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q\n---\n%s", want, got)
		}
	}
	// The two longer tokens must survive verbatim.
	for _, keep := range []string{"measured_run(1)", "remeasured"} {
		if !strings.Contains(got, keep) {
			t.Errorf("%q must be left alone\n---\n%s", keep, got)
		}
	}
	if strings.Contains(got, "default_caps_run") || strings.Contains(got, "redefault_caps") {
		t.Errorf("boundary rule leaked into a longer token:\n%s", got)
	}
	// No match at all ⇒ content byte-identical, count 0.
	same, m := RenameSymbolIn("nothing here\n", "measured", "default_caps")
	if m != 0 || same != "nothing here\n" {
		t.Errorf("no-match must be a no-op: n=%d content=%q", m, same)
	}
}

// TestRenameEnglishWordInComment: asserted, not assumed. Renaming `measured` DOES
// rewrite "measured against this host's `session_reports` table" — there the word is
// a whole word, so the identifier boundary cannot tell a symbol from English prose,
// and the tool does not parse. That is precisely why Invoke reports the file list and
// writes nothing until the caller says confirm: true.
func TestRenameEnglishWordInComment(t *testing.T) {
	const line = "// Compare measured against this host's `session_reports` table\n"
	got, n := RenameSymbolIn(line, "measured", "default_caps")
	if n != 1 {
		t.Fatalf("the English word must match as a whole word: n=%d", n)
	}
	want := "// Compare default_caps against this host's `session_reports` table\n"
	if got != want {
		t.Errorf("content = %q\nwant      %q", got, want)
	}
}

// TestRenameRejectsNonIdentifierArguments: `old` as a phrase matches nothing and
// reads as a silent no-op; `new` as `2bad` writes a file that cannot compile. Each is
// refused with an error naming the offending argument, before anything is read.
func TestRenameRejectsNonIdentifierArguments(t *testing.T) {
	ws, _ := wsAt(t)
	cases := []struct {
		name    string
		args    map[string]any
		wantArg string
	}{
		{"phrase old", map[string]any{"old": "measured table", "new": "default_caps"}, `"old"`},
		{"leading-digit new", map[string]any{"old": "measured", "new": "2bad"}, `"new"`},
		{"empty old", map[string]any{"old": "", "new": "default_caps"}, `"old"`},
		{"dotted old", map[string]any{"old": "SeatCaps::measured", "new": "default_caps"}, `"old"`},
		{"same name both ways", map[string]any{"old": "measured", "new": "measured"}, "both"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := renameSymbolTool{ws}.Invoke(context.Background(), Call{Name: NameRenameSymbol, Args: tc.args})
			if err == nil {
				t.Fatalf("want an error for %v", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantArg) {
				t.Errorf("error must name the offending argument (%s), got: %v", tc.wantArg, err)
			}
		})
	}
}

// TestRenameReportsWithoutWriting: report-first. Without confirm the file on disk is
// byte-identical afterwards, and the output names the file, the count, and the
// confirm: true re-issue. A repo-wide rename that writes on its first call can
// rewrite English prose in an unrelated file before the caller has seen the list.
func TestRenameReportsWithoutWriting(t *testing.T) {
	ws, root := wsAt(t)
	mk := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, p, content)
	}
	const capsSrc = "impl SeatCaps {\n    fn measured() -> Self {}\n}\n"
	const orig = "fn main() { let c = SeatCaps::measured(); }\n"
	mk("src/caps.rs", capsSrc)
	mk("src/main.rs", orig)
	mk("src/other.rs", "fn unrelated() {}\n")
	mk("target/gen.rs", "fn measured() {}\n") // build dir: never touched

	res, err := renameSymbolTool{ws}.Invoke(context.Background(), Call{
		Name: NameRenameSymbol,
		Args: map[string]any{"old": "measured", "new": "default_caps"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	out := res.Output
	for _, want := range []string{"src/caps.rs", "src/main.rs", "2 occurrence", "confirm: true", "NOTHING WAS WRITTEN"} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "target/gen.rs") {
		t.Errorf("build dirs must be skipped:\n%s", out)
	}

	// Nothing on disk changed.
	for _, f := range []struct{ rel, want string }{{"src/caps.rs", capsSrc}, {"src/main.rs", orig}, {"target/gen.rs", "fn measured() {}\n"}} {
		got, err := os.ReadFile(filepath.Join(root, f.rel))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != f.want {
			t.Errorf("%s changed without confirm:\n got %q\nwant %q", f.rel, got, f.want)
		}
	}
}

// TestRenameConfirmWritesTheFiles: with confirm: true the same call rewrites the
// files on disk and says which ones it wrote.
func TestRenameConfirmWritesTheFiles(t *testing.T) {
	ws, root := wsAt(t)
	mk := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, p, content)
	}
	mk("src/caps.rs", "impl SeatCaps {\n    fn measured() -> Self {}\n}\n")
	mk("src/main.rs", "fn main() { let c = SeatCaps::measured(); }\n")
	mk("src/notes.md", "measured against this host's table\n")

	res, err := renameSymbolTool{ws}.Invoke(context.Background(), Call{
		Name: NameRenameSymbol,
		Args: map[string]any{"old": "measured", "new": "default_caps", "confirm": true},
	})
	if err != nil {
		t.Fatalf("Invoke confirm: %v", err)
	}
	if !strings.Contains(res.Output, "wrote 3 file(s)") {
		t.Errorf("output must report the files written:\n%s", res.Output)
	}
	for f, want := range map[string]string{
		"src/caps.rs":  "impl SeatCaps {\n    fn default_caps() -> Self {}\n}\n",
		"src/main.rs":  "fn main() { let c = SeatCaps::default_caps(); }\n",
		"src/notes.md": "default_caps against this host's table\n",
	} {
		got, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s on disk = %q\nwant %q", f, got, want)
		}
	}
}

// TestRenameNamesThePathWhenNothingMatches: a rename that finds nothing says so and
// names the path searched — an empty observation sends the caller re-running the same
// call with a different path, which is the churn this tool exists to remove.
func TestRenameNamesThePathWhenNothingMatches(t *testing.T) {
	ws, root := wsAt(t)
	mustWrite(t, filepath.Join(root, "a.rs"), "fn untouched() {}\n")

	res, err := renameSymbolTool{ws}.Invoke(context.Background(), Call{
		Name: NameRenameSymbol,
		Args: map[string]any{"old": "measured", "new": "default_caps", "path": "a.rs"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(res.Output, "no occurrences") || !strings.Contains(res.Output, "a.rs") {
		t.Errorf("no-match result must say so and name the path searched: %q", res.Output)
	}
}

// TestRenameReportsACollisionWithoutRefusing: when the NEW name already appears as an
// identifier, the file is listed under a collision heading in BOTH modes and the call
// still succeeds — merging two symbols into one is a legitimate rename, so the caller
// decides, but it has to see it.
func TestRenameReportsACollisionWithoutRefusing(t *testing.T) {
	ws, root := wsAt(t)
	mustWrite(t, filepath.Join(root, "caps.rs"), "fn measured() {}\nfn default_caps() {}\n")

	for _, confirm := range []bool{false, true} {
		res, err := renameSymbolTool{ws}.Invoke(context.Background(), Call{
			Name: NameRenameSymbol,
			Args: map[string]any{"old": "measured", "new": "default_caps", "confirm": confirm},
		})
		if err != nil {
			t.Fatalf("confirm=%v must not refuse a collision: %v", confirm, err)
		}
		if !strings.Contains(res.Output, "COLLIDE") || !strings.Contains(res.Output, "caps.rs") {
			t.Errorf("confirm=%v: collision must be reported by file:\n%s", confirm, res.Output)
		}
	}
}

// TestRenameIsRegistered: the tool is in the default vocabulary, jailed like the rest,
// and cannot be pointed outside the workspace.
func TestRenameIsRegistered(t *testing.T) {
	ws, _ := wsAt(t)
	if _, ok := DefaultRegistry(ws).Lookup(NameRenameSymbol); !ok {
		t.Fatal("rename_symbol must be registered in the default registry")
	}
	if _, err := (renameSymbolTool{ws}).Invoke(context.Background(), Call{
		Name: NameRenameSymbol,
		Args: map[string]any{"old": "measured", "new": "default_caps", "path": "../../etc"},
	}); err == nil {
		t.Error("a path escaping the jail must be rejected")
	}
}
