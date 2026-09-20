package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestImportsOfNamesTheCodeBehindATest: measured on kloo-bench A33 — the model
// tried to edit the statutory TEST (so it had found the right area), was refused,
// and then spent the rest of the run editing payroll files while the red assertion
// was about statutory dedup. A refusal that only says "no" leaves it where it was;
// naming what the test imports points at the code.
func TestImportsOfNamesTheCodeBehindATest(t *testing.T) {
	root := t.TempDir()
	mk := func(rel, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("src/statutory/dedupe.ts", "export const dedupe = 1\n")
	mk("src/statutory/helpers/rows.ts", "export const rows = 1\n")
	mk("tests/statutory.test.ts", `
import { dedupe } from '../src/statutory/dedupe'
import { rows } from '../src/statutory/helpers/rows'
import { describe } from 'vitest'
const x = require('../src/statutory/dedupe')
`)
	l := &Loop{Root: root}
	got := l.importsOf("tests/statutory.test.ts")

	want := map[string]bool{"src/statutory/dedupe.ts": false, "src/statutory/helpers/rows.ts": false}
	for _, g := range got {
		if _, ok := want[g]; !ok {
			t.Fatalf("named something that is not a workspace file: %q (all: %#v)", g, got)
		}
		want[g] = true
	}
	for f, seen := range want {
		if !seen {
			t.Fatalf("did not name %s (got %#v)", f, got)
		}
	}
	// 'vitest' is a package, not a file in the repo — naming it would send the
	// model into node_modules.
	for _, g := range got {
		if g == "vitest" {
			t.Fatal("named a package as if it were a source file")
		}
	}
}

// TestImportsOfIsSafeOnJunk: an unreadable path, an empty path or a file with no
// resolvable imports must return nothing rather than guesses.
func TestImportsOfIsSafeOnJunk(t *testing.T) {
	l := &Loop{Root: t.TempDir()}
	for _, p := range []string{"", "does/not/exist.ts"} {
		if got := l.importsOf(p); len(got) != 0 {
			t.Fatalf("%q produced %#v", p, got)
		}
	}
}

// TestCorrectiveNamesWhatTheFailingTestExercises: A33 again — kloo edited
// thirteenth-month.ts four times (twice identically) and churned out, while grok
// passed by also changing the repository query and the resolver. The corrective
// has to name what the test pulls in, or "edit something" is all the model hears.
func TestCorrectiveNamesWhatTheFailingTestExercises(t *testing.T) {
	c := editCorrective(9, true,
		[]string{"dedupes divergent per-company statutory rows"},
		[]string{"src/modules-v2/payroll/queries.ts", "src/graphql/resolvers/payroll.ts"}).Content
	if !strings.Contains(c, "src/graphql/resolvers/payroll.ts") {
		t.Fatalf("corrective does not name the resolver the test exercises: %q", c)
	}
	if !strings.Contains(c, "belongs in one of these instead") {
		t.Fatalf("corrective does not connect the file list to the stuck edit: %q", c)
	}
}
