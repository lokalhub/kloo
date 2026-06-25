package repomap

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// TestWalkSkipsOversizeFiles is the OOM guard: a file larger than maxMappedFileBytes
// (e.g. a checked-in binary / model weight) must be skipped from the map so it is
// never read whole into memory. Small source beside it is still mapped.
func TestWalkSkipsOversizeFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "small.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "huge.gguf"), make([]byte, maxMappedFileBytes+1), 0o644); err != nil {
		t.Fatal(err)
	}
	nodes, err := Walk(dir)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	got := paths(nodes)
	for _, p := range got {
		if p == "huge.gguf" {
			t.Errorf("oversize file must be skipped from the map, got %v", got)
		}
	}
	small := false
	for _, p := range got {
		if p == "small.go" {
			small = true
		}
	}
	if !small {
		t.Errorf("small source file should still be mapped, got %v", got)
	}
}

func goFixture(t *testing.T) string {
	t.Helper()
	return "testdata/repos/go-fixture"
}

func paths(nodes []Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Path
	}
	return out
}

func TestWalkOrderedTreeAndSkips(t *testing.T) {
	nodes, err := Walk(goFixture(t))
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	// Golden ordered tree: dirs + files, lexicographic; hard-skips and
	// gitignored paths absent.
	want := []string{
		".gitignore",
		"go.mod",
		"internal",
		"internal/util",
		"internal/util/util.go",
		"main.go",
	}
	if got := paths(nodes); !reflect.DeepEqual(got, want) {
		t.Errorf("walk tree mismatch\n got: %v\nwant: %v", got, want)
	}
}

func TestWalkHardSkipsAbsent(t *testing.T) {
	nodes, _ := Walk(goFixture(t))
	for _, n := range nodes {
		for _, bad := range []string{".git", "node_modules", "dist", "bin"} {
			if n.Path == bad || hasPrefixSeg(n.Path, bad) {
				t.Errorf("hard-skip dir leaked into walk: %q", n.Path)
			}
		}
	}
}

func TestWalkGitignoreExcludes(t *testing.T) {
	nodes, _ := Walk(goFixture(t))
	for _, n := range nodes {
		if n.Path == "debug.log" {
			t.Error("*.log gitignore pattern not honoured (debug.log present)")
		}
		if n.Path == "build" || hasPrefixSeg(n.Path, "build") {
			t.Errorf("build/ gitignore pattern not honoured: %q", n.Path)
		}
	}
}

func TestWalkDeterministic(t *testing.T) {
	a, _ := Walk(goFixture(t))
	b, _ := Walk(goFixture(t))
	if !reflect.DeepEqual(paths(a), paths(b)) {
		t.Errorf("walk not deterministic:\n%v\n%v", paths(a), paths(b))
	}
}

func TestWalkNodesCarryMetadata(t *testing.T) {
	nodes, _ := Walk(goFixture(t))
	var sawFileWithSize, sawDir bool
	for _, n := range nodes {
		if n.IsDir && n.Path == "internal/util" {
			sawDir = true
		}
		if !n.IsDir && n.Path == "main.go" {
			if n.Size <= 0 {
				t.Errorf("main.go size = %d, want > 0", n.Size)
			}
			sawFileWithSize = true
		}
	}
	if !sawFileWithSize || !sawDir {
		t.Errorf("expected file-with-size and a dir node (file=%v dir=%v)", sawFileWithSize, sawDir)
	}
}

// hasPrefixSeg reports whether path has seg as its first path segment.
func hasPrefixSeg(path, seg string) bool {
	return path == seg || len(path) > len(seg) && path[:len(seg)+1] == seg+"/"
}

// TestWalkNestedGitignore: a .gitignore in a SUBDIR is honoured (git semantics),
// matched relative to that subdir — so a project nested under a root with no
// .gitignore of its own still excludes its build output. This is the Ionic case:
// kloo's root is a parent dir, the Angular project (and its /www, /.angular
// ignores) lives one level down.
func TestWalkNestedGitignore(t *testing.T) {
	root := t.TempDir()
	app := filepath.Join(root, "app")
	mustWrite(t, filepath.Join(app, ".gitignore"), "/www\n/.angular/cache\n")
	mustWrite(t, filepath.Join(app, "src", "main.ts"), "export const x = 1\n")
	mustWrite(t, filepath.Join(app, "www", "bundle.js"), "console.log(1)\n")
	mustWrite(t, filepath.Join(app, ".angular", "cache", "x.json"), "{}\n")

	got := paths(mustWalk(t, root))
	for _, p := range got {
		if contains(p, "www/") || contains(p, ".angular/") {
			t.Errorf("nested .gitignore not honoured: build output %q should be skipped\nall: %v", p, got)
		}
	}
	if !hasPath(got, "app/src/main.ts") {
		t.Errorf("real source under the nested project should be mapped, got %v", got)
	}
}

// TestWalkHardSkipsBuildDirs: build/cache dirs are skipped by NAME even with no
// .gitignore anywhere (the backstop), so a fresh build can't flood the map.
func TestWalkHardSkipsBuildDirs(t *testing.T) {
	root := t.TempDir()
	mustWrite(t, filepath.Join(root, "main.go"), "package x\n")
	for _, d := range []string{"www", ".angular", "node_modules", "dist", "build", "target", ".next"} {
		mustWrite(t, filepath.Join(root, d, "junk.js"), "x\n")
	}
	got := paths(mustWalk(t, root))
	for _, p := range got {
		for _, d := range []string{"www/", ".angular/", "node_modules/", "dist/", "build/", "target/", ".next/"} {
			if contains(p, d) {
				t.Errorf("hard-skip dir leaked into map: %q", p)
			}
		}
	}
	if !hasPath(got, "main.go") {
		t.Errorf("source should still be mapped, got %v", got)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustWalk(t *testing.T, root string) []Node {
	t.Helper()
	nodes, err := Walk(root)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	return nodes
}

func hasPath(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

func contains(s, sub string) bool {
	return len(sub) > 0 && filepath.ToSlash(s) != "" && strings.Contains(s, sub)
}
