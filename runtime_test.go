package daemon_test

import (
	"bufio"
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/malivvan/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeStoreWriteListAndRead(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	ep := daemon.Endpoint{Network: daemon.NetworkTCP, Address: "127.0.0.1:7474"}
	rec := daemon.NewRuntimeRecord("kata", "v1", ep)
	rec.StartedAt = time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	rec.Metadata = map[string]string{"db_path": "/tmp/kata.db"}

	path, err := store.Write(rec)
	require.NoError(err)
	assert.Equal("daemon."+strconv.Itoa(os.Getpid())+".json", filepath.Base(path))

	records, err := store.List()
	require.NoError(err)
	require.Len(records, 1)
	got := records[0]
	assert.Equal(os.Getpid(), got.PID)
	assert.Equal("kata", got.Service)
	assert.Equal("v1", got.Version)
	assert.Equal(ep, got.Endpoint())
	assert.Equal(path, got.SourcePath)
	assert.Equal("/tmp/kata.db", got.Metadata["db_path"])
}

func TestNewRuntimeRecordPersistsCurrentProcessIdentity(t *testing.T) {
	rec := daemon.NewRuntimeRecord("tool", "v1", daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1234",
	})
	assert.Equal(t, daemon.ProcessIdentityMatch,
		daemon.CompareRuntimeProcessIdentity(rec))
}

func TestRuntimeStoreCleanupDeadLeavesMismatchedFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	pid := deadPID(t)
	dead := daemon.RuntimeRecord{
		PID:       pid,
		Network:   daemon.NetworkTCP,
		Address:   "127.0.0.1:7474",
		StartedAt: time.Now(),
	}
	_, err := store.Write(dead)
	require.NoError(err)

	mismatchPath, err := store.Path(pid + 1)
	require.NoError(err)
	err = os.WriteFile(mismatchPath, []byte(`{"pid":`+strconv.Itoa(pid)+`,"address":"127.0.0.1:7475"}`), 0o644)
	require.NoError(err)

	removed, err := store.CleanupDead()
	require.NoError(err)
	assert.Equal(1, removed)
	deadPath, err := store.Path(pid)
	require.NoError(err)
	_, err = os.Stat(deadPath)
	assert.True(os.IsNotExist(err), "dead runtime still exists or unexpected stat error: %v", err)
	_, err = os.Stat(mismatchPath)
	require.NoError(err)
}

func TestRuntimeStoreRejectsPrefixTraversal(t *testing.T) {
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "../escape"}

	_, err := store.Path(123)
	require.Error(err)
	_, err = store.Write(daemon.RuntimeRecord{
		PID:     123,
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:7474",
	})
	require.Error(err)
	_, err = store.List()
	require.Error(err)
	_, err = store.CleanupDead()
	require.Error(err)
}

func TestRuntimeStoreRejectsRelativeDirBeforePreparing(t *testing.T) {
	assert := assert.New(t)

	store := daemon.RuntimeStore{Dir: "relative-runtime"}

	_, err := store.LockPath()
	require.Error(t, err)
	assert.Contains(err.Error(), "must be absolute")

	_, statErr := os.Stat("relative-runtime")
	assert.True(os.IsNotExist(statErr), "relative runtime dir should not be created: %v", statErr)
}

func TestRuntimeStoreListenLockPathIsSeparateFromStartLock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "kata"}

	startLock, err := store.LockPath()
	require.NoError(err)
	listenLock, err := store.ListenLockPath()
	require.NoError(err)

	assert.Equal(filepath.Join(store.Dir, "kata.lock"), startLock)
	assert.Equal(filepath.Join(store.Dir, "kata.listen.lock"), listenLock)
}

func TestRuntimeStoreCheckWritableLeavesNoFiles(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()
	store := daemon.RuntimeStore{Dir: dir}

	require.NoError(store.CheckWritable())
	entries, err := os.ReadDir(dir)
	require.NoError(err)
	require.Empty(entries)
	records, err := store.List()
	require.NoError(err)
	require.Empty(records)
}

func TestRuntimeStoreCheckWritableRejectsNonDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "runtime-file")
	require.NoError(os.WriteFile(path, []byte("not a directory"), 0o600))

	err := (daemon.RuntimeStore{Dir: path}).CheckWritable()

	require.Error(err)
	assert.Contains(err.Error(), "prepare runtime dir")
}

func TestRuntimeStoreIgnoresStagingFiles(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	pid := deadPID(t)
	// An interrupted Write leaves its staging file behind under this name.
	staging := filepath.Join(store.Dir, ".daemon."+strconv.Itoa(pid)+".json.tmp-123456")
	body := []byte(`{"pid":` + strconv.Itoa(pid) + `,"address":"127.0.0.1:7474"}`)
	require.NoError(os.WriteFile(staging, body, 0o644))

	records, err := store.List()
	require.NoError(err)
	assert.Empty(records)
	removed, err := store.CleanupDead()
	require.NoError(err)
	assert.Zero(removed)
	_, err = os.Stat(staging)
	require.NoError(err)

	_, err = store.Write(daemon.RuntimeRecord{PID: pid, Address: "127.0.0.1:7475"})
	require.NoError(err)
	entries, err := os.ReadDir(store.Dir)
	require.NoError(err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	assert.ElementsMatch([]string{".daemon." + strconv.Itoa(pid) + ".json.tmp-123456", "daemon." + strconv.Itoa(pid) + ".json"}, names)
}

func TestRuntimeStoreRejectsNonPortablePrefix(t *testing.T) {
	for _, prefix := range []string{"a:b", "NUL", "con", "CONIN$", "tool.", "tool "} {
		t.Run(prefix, func(t *testing.T) {
			store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: prefix}

			_, err := store.Path(123)

			require.ErrorIs(t, err, daemon.ErrNotPortable)
		})
	}
}

func TestRuntimeStorePrefixReservesPortableStagingSuffix(t *testing.T) {
	require := require.New(t)
	dir := t.TempDir()
	store := daemon.RuntimeStore{Dir: dir, Prefix: strings.Repeat("a", 214)}

	path, err := store.Write(daemon.RuntimeRecord{
		PID:     1,
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:7474",
	})
	require.NoError(err)
	_, err = os.Stat(path)
	require.NoError(err)

	store.Prefix = strings.Repeat("a", 215)
	_, err = store.Path(1)
	require.ErrorIs(err, daemon.ErrNotPortable)
}

func TestRuntimeProcessAliveKeepsRecordWithUnreadableVersionedIdentity(t *testing.T) {
	rec := daemon.RuntimeRecord{
		PID:               deadPID(t),
		Network:           daemon.NetworkTCP,
		Address:           "127.0.0.1:7474",
		ProcessIdentityV2: "future-v1:12345",
	}
	assert.True(t, daemon.RuntimeProcessAlive(rec))
}

func TestCleanupDeadKeepsRecordWithUnjudgedIdentity(t *testing.T) {
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	path, err := store.Write(daemon.RuntimeRecord{
		PID:               deadPID(t),
		Network:           daemon.NetworkTCP,
		Address:           "127.0.0.1:7474",
		Service:           "tool",
		StartedAt:         time.Now(),
		ProcessIdentityV2: "future-v1:12345",
	})
	require.NoError(t, err)

	removed, err := store.CleanupDead()
	require.NoError(t, err)
	assert.Zero(t, removed)
	assert.FileExists(t, path)
}

func TestDiscoverProbesRecordWithUnjudgedIdentity(t *testing.T) {
	pid := deadPID(t)
	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: "tool",
		PID:     pid,
	}))
	defer server.Close()
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:               pid,
		Network:           daemon.NetworkTCP,
		Address:           listenerAddr(t, server),
		Service:           "tool",
		StartedAt:         time.Now(),
		ProcessIdentityV2: "future-v1:12345",
	})
	require.NoError(t, err)

	rec, _, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "tool"},
		RequirePIDAlive: true,
	})
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, pid, rec.PID)
}

func TestTryAcquireStartLockCoordinatesWithManager(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	release, acquired, err := store.TryAcquireStartLock(t.Context())
	require.NoError(err)
	require.True(acquired)
	require.NotNil(release)
	// Keep cleanup valid even if a precondition below fails.
	defer func() {
		if release != nil {
			release()
		}
	}()

	again, acquired, err := store.TryAcquireStartLock(t.Context())
	require.NoError(err)
	require.False(acquired)
	require.Nil(again)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = store.AcquireStartLock(ctx)
	require.ErrorIs(err, context.Canceled)

	snapshot := filepath.Join(store.Dir, "progress.json")
	require.NoError(os.WriteFile(snapshot, []byte("old progress"), 0o600))
	ready := false
	manager := daemon.Manager{
		Store: store,
		FindFunc: func(context.Context) (daemon.RuntimeRecord, daemon.PingInfo, bool, error) {
			return daemon.RuntimeRecord{}, daemon.PingInfo{}, ready, nil
		},
		Start: func(ctx context.Context) error {
			// Manager owns the same local lock while invoking Start.
			probeRelease, acquired, err := store.TryAcquireStartLock(ctx)
			require.
				NoError(err)
			assert.False(acquired)
			assert.Nil(probeRelease)
			assert.NoFileExists(snapshot)
			ready = true
			return nil
		},
	}
	ctx, cancel = context.WithTimeout(t.Context(), 20*time.Millisecond) //nolint:kennlint // the deadline is the expected result; the test holds the start lock, so Ensure can only stop when it fires
	defer cancel()
	_, _, err = manager.Ensure(ctx, time.Second)
	require.ErrorIs(err, context.DeadlineExceeded)
	require.False(ready)
	require.NoError(os.Remove(snapshot)) // Caller-owned cleanup happens under lock.
	release()
	release = nil
	_, _, err = manager.Ensure(t.Context(), time.Second)
	require.NoError(err)
	require.True(ready)
	release, acquired, err = store.TryAcquireStartLock(t.Context())
	require.NoError(err)
	require.True(acquired)
}

func TestTryAcquireStartLockErrorAndRetry(t *testing.T) {
	require := require.New(t)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	path, err := store.LockPath()
	require.NoError(err)
	// Windows rejects opening a directory here. Unix permits locking one,
	// so use an unreadable file to exercise its open error instead.
	if runtime.GOOS == "windows" {
		require.NoError(os.Mkdir(path, 0o700))
	} else {
		if os.Geteuid() == 0 {
			t.Skip("root can open mode-000 files")
		}
		require.NoError(os.WriteFile(path, nil, 0o000))
	}
	release, acquired, err := store.TryAcquireStartLock(t.Context())
	require.Error(err)
	require.Nil(release)
	require.False(acquired)
	require.NoError(os.Remove(path))
	release, acquired, err = store.TryAcquireStartLock(t.Context())
	require.NoError(err)
	require.True(acquired)
	require.NotNil(release)
	release()
}

func TestTryAcquireStartLockCanceled(t *testing.T) {
	require := require.New(t)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	release, acquired, err := store.TryAcquireStartLock(ctx)
	require.ErrorIs(err, context.Canceled)
	require.Nil(release)
	require.False(acquired)
	release, acquired, err = store.TryAcquireStartLock(t.Context())
	require.NoError(err)
	require.True(acquired)
	release()
}

func TestTryAcquireStartLockAfterHolderExit(t *testing.T) {
	require := require.New(t)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	exe, err := os.Executable()
	require.NoError(err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestStartLockProcess$", "--", store.Dir)
	stdout, err := cmd.StdoutPipe()
	require.NoError(err)
	stdin, err := cmd.StdinPipe()
	require.NoError(err)
	defer stdin.Close()
	require.NoError(cmd.Start())
	waited := false
	defer func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(err)
	require.Equal("locked\n", line)
	release, acquired, err := store.TryAcquireStartLock(t.Context())
	require.NoError(err)
	require.False(acquired)
	require.Nil(release)
	require.NoError(cmd.Process.Kill())
	require.Error(cmd.Wait())
	waited = true
	path, err := store.LockPath()
	require.NoError(err)
	require.FileExists(path)
	release, acquired, err = store.TryAcquireStartLock(t.Context())
	require.NoError(err)
	require.True(acquired, "a terminated holder's remaining file does not block acquisition")
	require.NotNil(release)
	release()
}

func TestStartLockProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	store := daemon.RuntimeStore{Dir: os.Args[len(os.Args)-1]}
	release, err := store.AcquireStartLock(t.Context())
	require.NoError(t, err)
	defer release()
	_, err = os.Stdout.WriteString("locked\n")
	require.NoError(t, err)
	var buf [1]byte
	_, _ = os.Stdin.Read(buf[:])
}

// deadPID returns a PID that names no live process, or skips the test.
func deadPID(t *testing.T) int {
	t.Helper()
	const pid = 999999
	if daemon.ProcessAlive(pid) {
		t.Skipf("pid %d is alive on this host", pid)
	}
	return pid
}
