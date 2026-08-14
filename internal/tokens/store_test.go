package tokens

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStoreRoundTrip: a measured ratio survives to the next run.
func TestStoreRoundTrip(t *testing.T) {
	ws := t.TempDir()
	s := NewStore(ws)

	if got := s.Ratio("some-model"); got != 0 {
		t.Errorf("a cold store should report 0, got %v", got)
	}
	if err := s.Save("some-model", 3.75); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := NewStore(ws).Ratio("some-model"); got != 3.75 {
		t.Errorf("ratio = %v, want 3.75 read back from disk", got)
	}
	if got := NewStore(ws).Ratio("other-model"); got != 0 {
		t.Errorf("an unmeasured model should report 0, got %v", got)
	}
}

// TestStoreMergesModels: saving one model must not drop the others.
func TestStoreMergesModels(t *testing.T) {
	ws := t.TempDir()
	s := NewStore(ws)
	if err := s.Save("model-a", 3.5); err != nil {
		t.Fatal(err)
	}
	if err := s.Save("model-b", 2.0); err != nil {
		t.Fatal(err)
	}
	if got := s.Ratio("model-a"); got != 3.5 {
		t.Errorf("model-a ratio = %v, want 3.5 (clobbered by the second save)", got)
	}
	if got := s.Ratio("model-b"); got != 2.0 {
		t.Errorf("model-b ratio = %v, want 2.0", got)
	}
}

// TestStoreIgnoresUselessSaves: an uncalibrated run must not erase a good
// measurement from an earlier one.
func TestStoreIgnoresUselessSaves(t *testing.T) {
	ws := t.TempDir()
	s := NewStore(ws)
	if err := s.Save("m", 3.75); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []float64{0, -1} {
		if err := s.Save("m", bad); err != nil {
			t.Fatalf("Save(%v): %v", bad, err)
		}
	}
	if err := s.Save("", 3.0); err != nil {
		t.Fatalf("Save with no model: %v", err)
	}
	if got := s.Ratio("m"); got != 3.75 {
		t.Errorf("ratio = %v, want the earlier 3.75 preserved", got)
	}
}

// TestStoreClampsOnWrite: a wild ratio is bounded before it reaches disk, so a
// bad run can't poison every later one.
func TestStoreClampsOnWrite(t *testing.T) {
	ws := t.TempDir()
	s := NewStore(ws)
	if err := s.Save("m", 500); err != nil {
		t.Fatal(err)
	}
	if got := s.Ratio("m"); got != maxCharsPerToken {
		t.Errorf("ratio = %v, want clamped to %v", got, maxCharsPerToken)
	}
}

// TestStoreSurvivesCorruption: a truncated or garbage file is a cold start, not
// a failure — this is an optimisation, never a reason to break a run.
func TestStoreSurvivesCorruption(t *testing.T) {
	ws := t.TempDir()
	path := filepath.Join(ws, ".kloo", StoreFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"m": 3.7`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(ws)
	if got := s.Ratio("m"); got != 0 {
		t.Errorf("corrupt file should read as uncalibrated, got %v", got)
	}
	if err := s.Save("m", 3.75); err != nil {
		t.Fatalf("Save over a corrupt file should recover: %v", err)
	}
	if got := s.Ratio("m"); got != 3.75 {
		t.Errorf("ratio = %v, want 3.75 after recovery", got)
	}
}

// TestNilStoreIsSafe: callers pass a store built from a path that may not exist.
func TestNilStoreIsSafe(t *testing.T) {
	var s *Store
	if got := s.Ratio("m"); got != 0 {
		t.Errorf("nil store ratio = %v, want 0", got)
	}
	if err := s.Save("m", 3.0); err != nil {
		t.Errorf("nil store Save = %v, want nil", err)
	}
}
