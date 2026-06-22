package tools

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// newWS makes a Workspace rooted at a fresh temp dir (canonicalised).
func newWS(t *testing.T) (Workspace, string) {
	t.Helper()
	root := t.TempDir()
	// t.TempDir may itself sit under a symlink (e.g. macOS /var); canonicalise
	// so our expectations match Resolve's canonical root.
	canon, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	return ws, canon
}

func TestResolveInJail(t *testing.T) {
	ws, root := newWS(t)
	got, err := ws.Resolve("subdir/file.go")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := filepath.Join(root, "subdir", "file.go")
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestResolveRejectsTraversal(t *testing.T) {
	ws, _ := newWS(t)
	for _, rel := range []string{"../outside.txt", "a/../../outside.txt", "../../etc/passwd"} {
		if _, err := ws.Resolve(rel); !errors.Is(err, ErrPathEscape) {
			t.Errorf("Resolve(%q): want ErrPathEscape, got %v", rel, err)
		}
	}
}

func TestResolveRejectsAbsoluteEscape(t *testing.T) {
	ws, _ := newWS(t)
	if _, err := ws.Resolve("/etc/passwd"); !errors.Is(err, ErrPathEscape) {
		t.Errorf("want ErrPathEscape for /etc/passwd, got %v", err)
	}
}

func TestResolveAllowsAbsoluteInsideRoot(t *testing.T) {
	ws, root := newWS(t)
	abs := filepath.Join(root, "nested", "ok.go")
	got, err := ws.Resolve(abs)
	if err != nil {
		t.Fatalf("absolute-inside-root should resolve, got %v", err)
	}
	if got != filepath.Clean(abs) {
		t.Errorf("got %q, want %q", got, abs)
	}
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is privileged on windows")
	}
	ws, root := newWS(t)
	outside := t.TempDir() // a dir outside the workspace root
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	// A path that traverses the escaping symlink must be rejected, and the
	// out-of-jail target must never be returned.
	if got, err := ws.Resolve("escape/secret.txt"); !errors.Is(err, ErrPathEscape) {
		t.Errorf("want ErrPathEscape via symlink, got path=%q err=%v", got, err)
	}
}

func TestResolveAllowsSymlinkInsideRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation is privileged on windows")
	}
	ws, root := newWS(t)
	target := filepath.Join(root, "real")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "alias")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := ws.Resolve("alias/inside.txt"); err != nil {
		t.Errorf("symlink pointing inside root should resolve, got %v", err)
	}
}
