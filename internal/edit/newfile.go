package edit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// File and directory permissions for engine-created paths (decisions.md).
const (
	filePerm = 0o644
	dirPerm  = 0o755
)

// CreateFile implements the new-file (empty-SEARCH) form: it writes b.Replace to
// path, creating any missing parent directories.
//
// It REFUSES TO CLOBBER: if path already exists, it returns ErrFileExists and
// writes nothing. To change an existing file the caller must use a real
// SEARCH/REPLACE edit (ApplyToFile / Apply); the overwrite-allowed path is
// write_file in internal/tools, never this function. An empty b.Replace creates
// an empty file (documented, not an error).
func CreateFile(path string, b Block) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("edit: %s: %w", path, ErrFileExists)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("edit: stat %s: %w", path, err)
	}

	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, dirPerm); err != nil {
			return fmt.Errorf("edit: mkdir %s: %w", dir, err)
		}
	}

	if err := os.WriteFile(path, []byte(b.Replace), filePerm); err != nil {
		return fmt.Errorf("edit: create %s: %w", path, err)
	}
	return nil
}
