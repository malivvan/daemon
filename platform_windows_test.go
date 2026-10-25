//go:build windows

package daemon_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/malivvan/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testProcessQueryLimitedInformation = 0x1000
	testProcessTerminate               = 0x0001
	testSynchronize                    = 0x00100000
)

func TestProcessAliveRejectsTerminatedProcessWithRetainedHandle(t *testing.T) {
	require := require.New(t)
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run", "^TestProcessAliveHelper$")
	cmd.Env = append(os.Environ(), "KIT_PROCESS_ALIVE_HELPER=1")
	require.NoError(cmd.Start())
	finished := false
	t.Cleanup(func() {
		if !finished {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	handle, err := syscall.OpenProcess(
		testProcessQueryLimitedInformation|
			testProcessTerminate|
			testSynchronize,
		false,
		uint32(cmd.Process.Pid),
	)
	require.NoError(err)
	defer func() { _ = syscall.CloseHandle(handle) }()

	require.True(daemon.ProcessAlive(cmd.Process.Pid))
	require.NoError(syscall.TerminateProcess(handle, 1))
	waitResult, err := syscall.WaitForSingleObject(handle, uint32((5*time.Second)/time.Millisecond))
	require.NoError(err)
	require.Equal(uint32(syscall.WAIT_OBJECT_0), waitResult)
	require.Error(cmd.Wait())
	finished = true

	assert.False(t, daemon.ProcessAlive(cmd.Process.Pid))
}

func TestProcessAliveHelper(t *testing.T) {
	if os.Getenv("KIT_PROCESS_ALIVE_HELPER") == "" {
		t.Skip("helper process for TestProcessAliveRejectsTerminatedProcessWithRetainedHandle")
	}
	select {}
}

// TestStartDetachedConsoleInputHelper is not a real test:
// TestStartDetachedDoesNotExposeConsoleInput re-executes the test binary with
// -test.run targeting it so the detached child can report whether CONIN$
// exists in its own process.
func TestStartDetachedConsoleInputHelper(t *testing.T) {
	marker := os.Getenv("KIT_DAEMON_TEST_CONSOLE_MARKER")
	if marker == "" {
		t.Skip("helper process for TestStartDetachedDoesNotExposeConsoleInput")
	}
	file, err := os.OpenFile("CONIN$", os.O_RDONLY, 0)
	if err == nil {
		_ = file.Close()
		require.NoError(t, os.WriteFile(marker, []byte("console-input-present"), 0o600))
		return
	}
	require.NoError(t, os.WriteFile(marker, []byte("console-input-unavailable"), 0o600))
}

func TestStartDetachedDoesNotExposeConsoleInput(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	marker := filepath.Join(t.TempDir(), "console-marker")

	err = daemon.StartDetached(t.Context(), daemon.StartDetachedOptions{
		Executable: exe,
		Args:       []string{"-test.run", "^TestStartDetachedConsoleInputHelper$"},
		Env:        append(os.Environ(), "KIT_DAEMON_TEST_CONSOLE_MARKER="+marker),
	})
	require.NoError(t, err)

	got := waitForMarker(t, marker)
	require.Equal(t, "console-input-unavailable", strings.TrimSpace(got))
}

func waitForMarker(t *testing.T, marker string) string {
	t.Helper()
	var data []byte
	require.Eventually(t, func() bool {
		var err error
		data, err = os.ReadFile(marker)
		return err == nil
	}, 10*time.Second, 25*time.Millisecond, "detached child never wrote marker file")
	return string(data)
}
