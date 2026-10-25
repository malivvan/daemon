package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// errPublished marks a failure after the new content became visible at the
// target. Callers must not retry blindly: a retry rewrites content that is
// already visible.
var errPublished = errors.New("atomic write: published")

// errNotDurable marks a post-publication directory sync failure: the target
// already holds the new content, but a crash may still lose it. It wraps
// errPublished.
var errNotDurable = fmt.Errorf("%w but not confirmed durable", errPublished)

// writeAtomicFile atomically replaces the file at path with data. It stages
// data in a temporary file next to the target, fsyncs and closes it, renames
// it over path, and fsyncs path's directory. Readers see the old or the new
// content, never a partial file. Parent directories are not created.
//
// The staging name is ".<target>.tmp-*", so it stays outside the
// "<prefix>.<pid>.json" namespace that RuntimeStore matches.
//
// An error wrapping errPublished means the new content is already visible at
// path; see errPublished and errNotDurable.
var writeAtomicFile = func(path string, data []byte, perm os.FileMode) error {
	staged, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomic write %s: %w", path, err)
	}
	stagedPath := staged.Name()
	// A successful publish removes the staging name; any failure before that
	// must not leave it behind.
	published := false
	defer func() {
		if published {
			return
		}
		_ = staged.Close()
		_ = os.Remove(stagedPath)
	}()
	if err := staged.Chmod(perm); err != nil {
		return fmt.Errorf("atomic write %s: chmod staging file: %w", path, err)
	}
	if _, err := staged.Write(data); err != nil {
		return fmt.Errorf("atomic write %s: write staging file: %w", path, err)
	}
	if err := staged.Sync(); err != nil {
		return fmt.Errorf("atomic write %s: sync staging file: %w", path, err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("atomic write %s: close staging file: %w", path, err)
	}
	if err := publishFile(stagedPath, path); err != nil {
		return fmt.Errorf("atomic write %s: %w", path, err)
	}
	published = true
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: %w", errNotDurable, err)
	}
	return nil
}
