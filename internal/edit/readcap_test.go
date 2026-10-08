package edit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEditPathRefusesAnOversizedFile: the 5 MiB cap lives in tools.ReadFile, and the
// edit path never went through it — ApplyToFile and applyMulti called os.ReadFile
// directly, so edit_file and write_file on a huge file were entirely uncapped.
//
// The process RSS ceiling does NOT cover this, which is why it is capped rather than
// deferred with the other audit findings: the ceiling is sampled at the step
// boundary, so an allocation that begins and completes inside one tool call is
// invisible to it. ApplyBlock builds the result beside the input, so peak is ~3x the
// file size within that call.
//
// The file here is 6 MiB — just over the cap, and small enough that the test costs
// nothing. Nothing in this package ever allocates at the scale the cap defends
// against.
func TestEditPathRefusesAnOversizedFile(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "huge.lock")
	if err := os.WriteFile(big, []byte(strings.Repeat("a", maxEditFileBytes+1)), 0o644); err != nil {
		t.Fatal(err)
	}

	err := ApplyToFile(big, Block{Search: "aaa", Replace: "bbb"})
	if err == nil {
		t.Fatal("ApplyToFile read a file over the cap — this is the shape of the 44 GB OOM")
	}
	// The refusal must be actionable: it lands in front of a model that will
	// otherwise retry the identical call.
	for _, want := range []string{"cap", "too large"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must say why and what to do instead; got %q", err)
		}
	}
	// And it must be a refusal, not a partial write.
	info, serr := os.Stat(big)
	if serr != nil || info.Size() != int64(maxEditFileBytes+1) {
		t.Errorf("the file was modified by a refused edit: %v %v", info, serr)
	}

	// stageFile is the second uncapped site (multi.go) and must refuse identically.
	if _, err := stageFile(big, []Block{{Search: "aaa", Replace: "bbb"}}); err == nil {
		t.Error("the multi-block staging path read a file over the cap")
	}

	// A file UNDER the cap still edits normally — the cap must not break ordinary work.
	small := filepath.Join(dir, "small.go")
	if err := os.WriteFile(small, []byte("package a\n\nconst x = 41\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ApplyToFile(small, Block{Search: "41", Replace: "42"}); err != nil {
		t.Fatalf("an ordinary edit was refused: %v", err)
	}
	got, _ := os.ReadFile(small)
	if !strings.Contains(string(got), "42") {
		t.Errorf("the edit did not apply: %q", got)
	}
}

// TestEditReadCapMatchesReadFileCap pins the two caps together. internal/edit sits
// below internal/tools in the dependency order so it cannot import the constant; a
// drift between them would mean read_file and edit_file disagree about what is too
// large, which is how the gap appeared in the first place.
func TestEditReadCapMatchesReadFileCap(t *testing.T) {
	const toolsMaxReadFileBytes = 5 << 20 // tools/files.go:40
	if maxEditFileBytes != toolsMaxReadFileBytes {
		t.Errorf("maxEditFileBytes = %d but tools.maxReadFileBytes = %d; the edit path and read_file must agree",
			maxEditFileBytes, toolsMaxReadFileBytes)
	}
}
