package repomap

import (
	"reflect"
	"testing"
)

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
