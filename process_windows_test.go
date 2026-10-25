//go:build windows

package daemon_test

import (
	"os"
	"os/exec"
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
