package daemon

import "fmt"

// Kill terminates pid immediately, without giving it a chance to clean up:
// SIGKILL on Unix and TerminateProcess on Windows.
//
// Kill only signals the PID the caller supplies; it does not verify the
// process's creation identity first. A caller holding a RuntimeRecord must
// establish that the live process is the recorded one (for example with
// CompareRuntimeProcessIdentity) before calling Kill, because an unknown
// identity never authorizes destructive action against a process.
func Kill(pid int) error {
	if pid <= 0 {
		return fmt.Errorf("kill process: pid must be > 0: %d", pid)
	}
	return killProcess(pid)
}
