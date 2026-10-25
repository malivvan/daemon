//go:build !windows

package daemon

import (
	"fmt"
	"os"
)

// publishFile renames staging over target, atomically replacing an existing
// file. It never copies, so a rename across volumes fails.
func publishFile(staging, target string) error {
	return os.Rename(staging, target)
}

// syncDir fsyncs dir so a rename or new file survives a crash. POSIX allows
// opening a directory read-only and syncing the resulting descriptor.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open directory %s for sync: %w", dir, err)
	}
	syncErr := d.Sync()
	closeErr := d.Close()
	if syncErr != nil {
		return fmt.Errorf("sync directory %s: %w", dir, syncErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close directory %s after sync: %w", dir, closeErr)
	}
	return nil
}
