//go:build windows

package daemon

import (
	"fmt"
	"syscall"
)

// processTerminate is the access right required to end a process.
const processTerminate = 0x0001

func killProcess(pid int) error {
	handle, err := syscall.OpenProcess(processTerminate, false, uint32(pid))
	if err != nil {
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer func() { _ = syscall.CloseHandle(handle) }()

	// Exit code 1 matches os.Process.Kill, so an externally terminated daemon is
	// not mistaken for one that exited successfully.
	if err := syscall.TerminateProcess(handle, 1); err != nil {
		return fmt.Errorf("kill process %d: %w", pid, err)
	}
	return nil
}
