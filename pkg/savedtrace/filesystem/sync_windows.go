package filesystem

import (
	"fmt"
	"os"
)

// Windows does not support File.Sync on the read-only directory handles opened
// by os.Open. File contents are still explicitly synced before close/rename.
// Directory entry durability across power loss is not guaranteed on Windows;
// the filesystem store documents this separately from ordinary process recovery.
func syncDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("sync path is not a directory")
	}
	return nil
}
