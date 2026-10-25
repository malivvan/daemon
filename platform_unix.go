//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

func detachChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// ProcessAlive reports whether pid appears to name a live process.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

func killProcess(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := process.Signal(os.Kill); err != nil {
		return fmt.Errorf("kill process %d: %w", pid, err)
	}
	return nil
}

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

// lockFilePlatform takes an exclusive flock lock on file. With nonblocking it
// reports false instead of waiting when another holder has the lock.
func lockFilePlatform(file *os.File, nonblocking bool) (bool, error) {
	how := syscall.LOCK_EX
	if nonblocking {
		how |= syscall.LOCK_NB
	}
	err := syscall.Flock(int(file.Fd()), how)
	if err == nil {
		return true, nil
	}
	if nonblocking && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
		return false, nil
	}
	return false, err
}

func unlockFilePlatform(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

// isStaleUnixSocketDialError reports whether err means the socket path has no
// listener.
func isStaleUnixSocketDialError(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT)
}

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
