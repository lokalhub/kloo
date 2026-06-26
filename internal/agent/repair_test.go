package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lokalhub/kloo/internal/llm"
)

// diffBlock renders a bare SEARCH/REPLACE diff (the shape edit_file receives in
// its "diff" arg) for the given search/replace bodies.
func diffBlock(search, replace string) string {
	return "<<<<<<< SEARCH\n" + search + "=======\n" + replace + ">>>>>>> REPLACE\n"
}

func writeTemp(t *testing.T, name, content string) (root, path string) {
	t.Helper()
	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, name
}

// O1 — no-match: the observation names the failing block, the reason, the actual
// content, and the fix instruction, with the user role.
func TestBuildRepairObservation_NotFound(t *testing.T) {
	root, path := writeTemp(t, "answer.txt", "wrong\n")
	diff := diffBlock("WRONG\n", "right\n")

	msg, ok := buildRepairObservation(root, path, diff)
	if !ok {
		t.Fatal("expected ok=true for a no-match diff")
	}
	if msg.Role != llm.RoleUser {
		t.Errorf("Role = %q, want %q", msg.Role, llm.RoleUser)
	}
	for _, want := range []string{"Failing SEARCH block", "not-found", "WRONG", "wrong", "Fix this edit"} {
		if !strings.Contains(msg.Content, want) {
			t.Errorf("observation missing %q\n---\n%s", want, msg.Content)
		}
	}
}

// O2 — ambiguous: the observation names the block as ambiguous, shows actual
// content, and instructs the model to make the SEARCH more specific.
func TestBuildRepairObservation_Ambiguous(t *testing.T) {
	root, path := writeTemp(t, "dup.txt", "x\nx\n")
	diff := diffBlock("x\n", "y\n")

	msg, ok := buildRepairObservation(root, path, diff)
	if !ok {
		t.Fatal("expected ok=true for an ambiguous diff")
	}
	for _, want := range []string{"Failing SEARCH block", "ambiguous", "make the SEARCH more specific", "Fix this edit"} {
		if !strings.Contains(msg.Content, want) {
			t.Errorf("observation missing %q\n---\n%s", want, msg.Content)
		}
	}
}

// O3a — oversize: a file above the read_file cap falls back to the bare
// observation (no panic, no unbounded read).
func TestBuildRepairObservation_OversizeFallsBack(t *testing.T) {
	root := t.TempDir()
	path := "big.txt"
	// 5 MiB + 1 byte — just over maxReadFileBytes.
	big := make([]byte, (5<<20)+1)
	for i := range big {
		big[i] = 'a'
	}
	if err := os.WriteFile(filepath.Join(root, path), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := buildRepairObservation(root, path, diffBlock("nope\n", "x\n")); ok {
		t.Error("expected ok=false for an oversize file (caller uses bare observation)")
	}
}

// O3b — unreadable / unparsable: a missing file falls back; a diff with no
// SEARCH/REPLACE block falls back.
func TestBuildRepairObservation_UnreadableOrUnparsable(t *testing.T) {
	root := t.TempDir()
	if _, ok := buildRepairObservation(root, "ghost.txt", diffBlock("a\n", "b\n")); ok {
		t.Error("expected ok=false for a missing file")
	}

	root2, p2 := writeTemp(t, "g.txt", "hello\n")
	if _, ok := buildRepairObservation(root2, p2, "this is just prose, no markers"); ok {
		t.Error("expected ok=false for a diff with no SEARCH/REPLACE block")
	}
}

// multi-block: a diff with one matching + one non-matching block names only the
// failing block; the actual content is shown; ok=true.
func TestBuildRepairObservation_MultiBlock(t *testing.T) {
	root, path := writeTemp(t, "code.txt", "keep me\nold line\n")
	// Block 1 matches ("keep me"); block 2 does not ("MISSING").
	diff := diffBlock("keep me\n", "kept\n") + diffBlock("MISSING\n", "new\n")

	msg, ok := buildRepairObservation(root, path, diff)
	if !ok {
		t.Fatal("expected ok=true when at least one block fails")
	}
	if !strings.Contains(msg.Content, "MISSING") {
		t.Errorf("observation should name the failing block (MISSING)\n---\n%s", msg.Content)
	}
	// Exactly one failing block reported.
	if n := strings.Count(msg.Content, "Failing SEARCH block"); n != 1 {
		t.Errorf("expected exactly 1 failing block reported, got %d\n---\n%s", n, msg.Content)
	}
	if !strings.Contains(msg.Content, "old line") {
		t.Errorf("observation should include the actual content\n---\n%s", msg.Content)
	}
}

// O5 — malformed nudge now inlines the file's actual content so the model can
// anchor its SEARCH to real text (botched boundaries, not just bad markers).
func TestBuildMalformedCorrection_IncludesFileContent(t *testing.T) {
	root, path := writeTemp(t, "login.page.ts", "@Component({\n  styles: `x`,\n})\nexport class LoginPage {}\n")
	msg := buildMalformedCorrection(root, path)
	for _, want := range []string{
		"MALFORMED", "<<<<<<< SEARCH", "=======", ">>>>>>> REPLACE", // format guidance
		"Actual current contents", "export class LoginPage {}", "BYTE-FOR-BYTE", // the inlined file
	} {
		if !strings.Contains(msg.Content, want) {
			t.Errorf("malformed nudge missing %q\n---\n%s", want, msg.Content)
		}
	}
	if msg.Role != llm.RoleUser {
		t.Errorf("role = %q, want user", msg.Role)
	}

	// Empty target → tell it to write_file instead.
	root2, path2 := writeTemp(t, "empty.ts", "")
	if got := buildMalformedCorrection(root2, path2).Content; !strings.Contains(got, "EMPTY") || !strings.Contains(got, "write_file") {
		t.Errorf("empty-file malformed nudge should suggest write_file: %q", got)
	}
}
