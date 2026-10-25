//go:build !windows

package daemon_test

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/malivvan/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultSocketPathRepairsPublicTempFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	tempDir := "/tmp"
	service := fmt.Sprintf("kitdpublic%d", os.Getpid())
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("XDG_RUNTIME_DIR", "")
	parent := filepath.Join(tempDir, fmt.Sprintf("%s-%d", service, os.Getuid()))
	t.Cleanup(func() { _ = os.RemoveAll(parent) })
	require.NoError(os.MkdirAll(parent, 0o700))
	require.NoError(os.Chmod(parent, 0o777))

	socketPath := daemon.DefaultSocketPath(service)
	require.NotEmpty(socketPath)
	assert.Equal(filepath.Join(parent, "daemon.sock"), socketPath)
	info, err := os.Stat(parent)
	require.NoError(err)
	assert.Zero(info.Mode().Perm() & 0o077)
}

func TestDefaultSocketPathRejectsSymlinkedTempFallback(t *testing.T) {
	tempDir := "/tmp"
	service := fmt.Sprintf("kitdsymlink%d", os.Getpid())
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("XDG_RUNTIME_DIR", "")
	parent := filepath.Join(tempDir, fmt.Sprintf("%s-%d", service, os.Getuid()))
	target := filepath.Join(tempDir, service+"-target")
	t.Cleanup(func() {
		_ = os.Remove(parent)
		_ = os.RemoveAll(target)
	})
	require.NoError(t, os.MkdirAll(target, 0o700))
	require.NoError(t, os.Symlink(target, parent))

	assert.Empty(t, daemon.DefaultSocketPath(service))
}

func TestDefaultSocketPathCreatesPrivateXDGRuntimeDir(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	xdg := filepath.Join("/tmp", fmt.Sprintf("kitdxdg%d", os.Getpid()))
	service := "svc"
	t.Cleanup(func() { _ = os.RemoveAll(xdg) })
	require.NoError(os.MkdirAll(xdg, 0o700))
	require.NoError(os.Chmod(xdg, 0o700))
	t.Setenv("XDG_RUNTIME_DIR", xdg)

	socketPath := daemon.DefaultSocketPath(service)
	require.NotEmpty(socketPath)
	parent := filepath.Join(xdg, service)
	assert.Equal(filepath.Join(parent, "daemon.sock"), socketPath)

	info, err := os.Stat(parent)
	require.NoError(err)
	assert.True(info.IsDir())
	assert.Zero(info.Mode().Perm() & 0o077)
}

func TestListenUnixRejectsUnsafeLockDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := staleUnixSocket(t)
	base, err := os.MkdirTemp("/tmp", "kitd-lock") //nolint:usetesting // unix socket paths must stay short, so the test needs a fixed OS temp root
	require.NoError(err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	target := filepath.Join(base, "target")
	link := filepath.Join(base, "link")
	require.NoError(os.MkdirAll(target, 0o700))
	require.NoError(os.Symlink(target, link))

	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}
	listener, err := daemon.Listen(t.Context(), ep, daemon.WithListenLockPath(filepath.Join(link, "daemon.lock")))

	require.Error(err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "prepare daemon lock dir")
	assert.Contains(err.Error(), "symlink")
	_, statErr := os.Lstat(socketPath)
	require.NoError(statErr, "stale socket should not be touched when lock dir is unsafe")
}

func TestListenUnixRejectsRelativeLockPath(t *testing.T) {
	assert := assert.New(t)

	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: unixSocketPath(t)}
	listener, err := daemon.Listen(t.Context(), ep, daemon.WithListenLockPath("daemon.lock"))

	require.Error(t, err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "daemon lock path")
	assert.Contains(err.Error(), "must be absolute")
}

func TestListenUnixRejectsRelativeSocketPath(t *testing.T) {
	assert := assert.New(t)

	lockPath := filepath.Join(t.TempDir(), "daemon.lock")
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: "daemon.sock"}
	listener, err := daemon.Listen(t.Context(), ep, daemon.WithListenLockPath(lockPath))

	require.Error(t, err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "unix socket path")
	assert.Contains(err.Error(), "must be absolute")
}

func TestListenUnixRejectsSharedSocketDirectoryEvenWithStoreLock(t *testing.T) {
	assert := assert.New(t)

	socketPath := filepath.Join("/tmp", "kitd-shared-socket.sock")
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}
	listener, err := daemon.Listen(t.Context(), ep, daemon.WithRuntimeStore(daemon.RuntimeStore{Dir: t.TempDir()}))

	require.Error(t, err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "validate unix socket dir")
	_, statErr := os.Lstat(socketPath)
	assert.True(os.IsNotExist(statErr), "socket in shared dir should not be created: %v", statErr)
}

func TestRuntimeStoreRepairsPublicDir(t *testing.T) {
	require := require.New(t)

	dir := filepath.Join("/tmp", fmt.Sprintf("kit-runtime-public-%d", os.Getpid()))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(os.RemoveAll(dir))
	require.NoError(os.MkdirAll(dir, 0o700))
	require.NoError(os.Chmod(dir, 0o777))

	store := daemon.RuntimeStore{Dir: dir}
	rec := daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:7474",
	}
	_, err := store.Write(rec)
	require.NoError(err)
	_, err = store.List()
	require.NoError(err)
	_, err = store.CleanupDead()
	require.NoError(err)
	_, err = store.LockPath()
	require.NoError(err)
	_, err = store.Read(filepath.Join(dir, "daemon.1.json"))
	require.Error(err)
	info, err := os.Stat(dir)
	require.NoError(err)
	require.Zero(info.Mode().Perm() & 0o077)
}

func TestRuntimeStoreRepairsPrivateUnusableDir(t *testing.T) {
	require := require.New(t)

	dir := filepath.Join("/tmp", fmt.Sprintf("kit-runtime-unusable-%d", os.Getpid()))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(os.RemoveAll(dir))
	require.NoError(os.MkdirAll(dir, 0o700))
	require.NoError(os.Chmod(dir, 0o500))

	store := daemon.RuntimeStore{Dir: dir}
	_, err := store.LockPath()
	require.NoError(err)
	info, err := os.Stat(dir)
	require.NoError(err)
	require.Equal(os.FileMode(0o700), info.Mode().Perm())
}

func TestRuntimeStoreCheckWritablePreservesPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write through directory mode restrictions")
	}
	require := require.New(t)

	parent := t.TempDir()
	require.NoError(os.Chmod(parent, 0o500))
	t.Cleanup(func() { require.NoError(os.Chmod(parent, 0o700)) })

	err := (daemon.RuntimeStore{Dir: filepath.Join(parent, "runtime")}).CheckWritable()

	require.ErrorIs(err, os.ErrPermission)
}

func TestRuntimeStoreRejectsSymlinkDir(t *testing.T) {
	require := require.New(t)

	base := filepath.Join("/tmp", fmt.Sprintf("kit-runtime-symlink-%d", os.Getpid()))
	target := base + "-target"
	t.Cleanup(func() {
		_ = os.Remove(base)
		_ = os.RemoveAll(target)
	})
	require.NoError(os.RemoveAll(base))
	require.NoError(os.RemoveAll(target))
	require.NoError(os.MkdirAll(target, 0o700))
	require.NoError(os.Symlink(target, base))

	store := daemon.RuntimeStore{Dir: base}
	_, err := store.LockPath()
	require.Error(err)
	_, err = store.Write(daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:7474",
	})
	require.Error(err)
	_, err = store.List()
	require.Error(err)
	_, err = store.CleanupDead()
	require.Error(err)
}

func TestRuntimeStoreRejectsSymlinkRecord(t *testing.T) {
	require := require.New(t)

	dir := filepath.Join("/tmp", fmt.Sprintf("kit-runtime-record-symlink-%d", os.Getpid()))
	target := filepath.Join(dir, "target.json")
	link := filepath.Join(dir, "daemon.1.json")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(os.RemoveAll(dir))
	require.NoError(os.MkdirAll(dir, 0o700))
	require.NoError(os.WriteFile(target, []byte(`{"pid":1,"address":"127.0.0.1:7474"}`), 0o644))
	require.NoError(os.Symlink(target, link))

	store := daemon.RuntimeStore{Dir: dir}
	_, err := store.Read(link)
	require.Error(err)
}

func TestRuntimeStoreRejectsNonRegularRecord(t *testing.T) {
	require := require.New(t)

	dir := filepath.Join("/tmp", fmt.Sprintf("kit-runtime-fifo-%d", os.Getpid()))
	record := filepath.Join(dir, "daemon.1.json")
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	require.NoError(os.RemoveAll(dir))
	require.NoError(os.MkdirAll(dir, 0o700))
	require.NoError(syscall.Mkfifo(record, 0o600))

	store := daemon.RuntimeStore{Dir: dir}
	_, err := store.Read(record)
	require.Error(err)
}
