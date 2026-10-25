//go:build windows

package daemon

// publishFile renames staging over target with MoveFileEx, which replaces an
// existing file and writes the rename through. It never copies, so a rename
// across volumes fails.
func publishFile(staging, target string) error {
	return moveFileEx(staging, target, moveFileReplaceExisting|moveFileWriteThrough)
}

// syncDir is a no-op on Windows: a directory handle cannot be fsynced to force
// entry changes to disk, and renames are write-through.
func syncDir(string) error {
	return nil
}
