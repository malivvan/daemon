//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
)

// ensurePrivateRuntimeDir creates path when needed and verifies it is a
// non-symlink directory owned by the current user with mode 0700.
func ensurePrivateRuntimeDir(path string) error {
	if path == "" {
		return errors.New("path is empty")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", path)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: missing owner information", path)
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s is not owned by current user", path)
	}
	if info.Mode().Perm() == 0o700 {
		return nil
	}
	// Open the directory without following a link and fix its mode through the
	// verified handle, so a swapped path cannot redirect the chmod.
	dir, err := os.OpenFile(path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("open private dir: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Chmod(0o700); err != nil {
		return fmt.Errorf("chmod private dir: %w", err)
	}
	info, err = dir.Stat()
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s is not mode 0700", path)
	}
	info, err = os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	return nil
}

// validatePrivateRuntimeDir verifies path is a non-symlink directory owned by
// the current user with mode 0700. It never creates or chmods the directory.
func validatePrivateRuntimeDir(path string) error {
	if path == "" {
		return errors.New("path is empty")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: missing owner information", path)
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s is not owned by current user", path)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s is not mode 0700", path)
	}
	return nil
}

// openRuntimeFile opens path without following symlinks and verifies the
// opened handle is a regular file owned by the current user.
func openRuntimeFile(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("path is empty")
	}
	file, err := os.OpenFile(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	if err := validateCurrentUserFile(file); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func validateCurrentUserFile(file *os.File) error {
	if file == nil {
		return errors.New("file is nil")
	}
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", file.Name())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("stat %s: missing owner information", file.Name())
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s is not owned by current user", file.Name())
	}
	return nil
}

// runtimeUID returns a stable filesystem-safe identifier for the current user.
func runtimeUID() string {
	return strconv.Itoa(os.Getuid())
}
