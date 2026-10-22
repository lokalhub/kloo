package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMapIndexIsCachedPerRoot: the walk and symbol extraction are the expensive
// half of assembling the map and they do not depend on the task or the budget, so
// they must run once per root rather than once per turn.
func TestMapIndexIsCachedPerRoot(t *testing.T) {
	root := genRepo(t, 4, 3)
	l := &Loop{Root: root}

	files1, syms1 := l.mapIndex()
	if len(files1) == 0 || len(syms1) == 0 {
		t.Fatal("mapIndex returned nothing for a populated repo")
	}
	// Poison the cache's inputs: if the walk ran again it would see this file.
	if err := os.WriteFile(filepath.Join(root, "zzz_new.go"), []byte("package pkg\n\nfunc BrandNew() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	files2, syms2 := l.mapIndex()
	if len(files2) != len(files1) || len(syms2) != len(syms1) {
		t.Fatalf("mapIndex re-walked the tree: %d/%d files, %d/%d symbol files",
			len(files1), len(files2), len(syms1), len(syms2))
	}
}

// TestMapIndexDropsOnTreeChange: a file this run creates must appear in the map
// on the next turn. The walk is reused across read-only turns, but a mutating
// turn bumps mapGen and forces a re-walk — otherwise a created file is invisible
// to the map for the rest of the run.
func TestMapIndexDropsOnTreeChange(t *testing.T) {
	root := genRepo(t, 3, 2)
	l := &Loop{Root: root}
	before, _ := l.mapIndex()
	if err := os.WriteFile(filepath.Join(root, "zzz_new.go"), []byte("package pkg\n\nfunc BrandNew() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.noteTreeChange()
	after, _ := l.mapIndex()
	if len(after) != len(before)+1 {
		t.Fatalf("created file missing from the map: %d files before, %d after", len(before), len(after))
	}
}

// TestMapIndexDropsOnRootChange: a Loop reused across workspaces must not serve
// the previous workspace's tree.
func TestMapIndexDropsOnRootChange(t *testing.T) {
	a, b := genRepo(t, 3, 2), genRepo(t, 7, 2)
	l := &Loop{Root: a}
	fa, _ := l.mapIndex()
	l.Root = b
	fb, _ := l.mapIndex()
	if len(fa) == len(fb) {
		t.Fatalf("cache survived a root change: %d files for both", len(fa))
	}
}

// TestMapIndexNoRootIsEmpty: no root degrades to no map, never a panic.
func TestMapIndexNoRootIsEmpty(t *testing.T) {
	l := &Loop{}
	if files, syms := l.mapIndex(); files != nil || syms != nil {
		t.Fatal("mapIndex with no root returned a map")
	}
}
