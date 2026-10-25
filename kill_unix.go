//go:build !windows

package daemon

import (
	"fmt"
	"os"
)

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
