package tokens

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Store persists the measured chars-per-token ratio per model, so calibration
// survives across runs.
//
// Without it every run starts from the 4.0 guess and only becomes accurate after
// its first turn — and `kloo tokens`, which deliberately makes no network call,
// could never be accurate at all. One number per model is enough: the ratio is a
// property of the tokenizer, not of the task.
//
// Lives in {workspace}/.kloo/, which already self-ignores from git.
type Store struct {
	mu   sync.Mutex
	path string
}

// StoreFile is the calibration file's name inside .kloo/.
const StoreFile = "token-calibration.json"

// NewStore returns the calibration store for a workspace.
func NewStore(workspace string) *Store {
	return &Store{path: filepath.Join(workspace, ".kloo", StoreFile)}
}

// Ratio returns the persisted ratio for a model, or 0 when there is none. Any
// read/parse failure is reported as 0 — a missing calibration is a cold start,
// never an error worth failing a run over.
func (s *Store) Ratio(model string) float64 {
	if s == nil || model == "" {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadLocked()
	if err != nil {
		return 0
	}
	return Clamp0(m[model])
}

// Save records the ratio measured for a model, merging into whatever is already
// stored. A non-positive ratio is ignored, so an uncalibrated run cannot erase a
// good measurement from an earlier one.
func (s *Store) Save(model string, ratio float64) error {
	if s == nil || model == "" || ratio <= 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.loadLocked()
	if err != nil || m == nil {
		m = map[string]float64{}
	}
	m[model] = Clamp(ratio)

	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Write-and-rename so a crash mid-write cannot leave a truncated file that
	// every later run then fails to parse.
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) loadLocked() (map[string]float64, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var m map[string]float64
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// Clamp0 clamps a ratio to the sane band but passes 0 through unchanged, so
// "no measurement" stays distinguishable from "a measurement at the floor".
func Clamp0(ratio float64) float64 {
	if ratio <= 0 {
		return 0
	}
	return Clamp(ratio)
}
