package daemon_test

import (
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/malivvan/daemon"
	"github.com/stretchr/testify/require"
)

// TestKillHelper is not a real test: TestKillTerminatesProcess re-executes the
// test binary with -test.run targeting it so Kill has a live process to end.
func TestKillHelper(t *testing.T) {
	if os.Getenv("KIT_DAEMON_TEST_KILL_HELPER") == "" {
		t.Skip("helper process for TestKillTerminatesProcess")
	}
	time.Sleep(time.Minute)
}

func TestKillTerminatesProcess(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(exe, "-test.run", "^TestKillHelper$")
	cmd.Env = append(os.Environ(), "KIT_DAEMON_TEST_KILL_HELPER=1")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	pid := cmd.Process.Pid
	require.True(t, daemon.ProcessAlive(pid))
	require.NoError(t, daemon.Kill(pid))

	require.Error(t, cmd.Wait())
	require.False(t, daemon.ProcessAlive(pid))
}

func TestKillRejectsNonPositivePID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		require.Error(t, daemon.Kill(pid), "pid %d must be rejected before any signal is sent", pid)
	}
	// A zero PID names the caller's process group on Unix; the guard must keep
	// this process alive.
	require.True(t, daemon.ProcessAlive(os.Getpid()))
}
