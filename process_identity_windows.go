//go:build windows

package daemon

import (
	"strconv"
	"syscall"
)

// processQueryLimitedInformation opens a process with the least access that
// still allows its exit and creation times to be read.
const processQueryLimitedInformation = 0x1000

// ReadProcessIdentity returns pid's OS-reported creation time as an opaque
// identity. ok is false when pid is gone or its creation time cannot be read.
func ReadProcessIdentity(pid int) (ProcessIdentity, bool) {
	if pid <= 0 {
		return "", false
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return "", false
	}
	defer func() { _ = syscall.CloseHandle(handle) }()
	var creation, exit, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(handle, &creation, &exit, &kernel, &user); err != nil {
		return "", false
	}
	// Unix milliseconds, the same encoding the previous Unix-based lookup used,
	// so identities recorded by older versions stay comparable.
	millis := creation.Nanoseconds() / 1e6
	if millis <= 0 {
		return "", false
	}
	return ProcessIdentity(strconv.FormatInt(millis, 10)), true
}
