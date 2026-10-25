//go:build !linux && !darwin && !windows

package daemon

// ReadProcessIdentity reports that this platform cannot inspect process
// creation identity. An unknown identity never authorizes destructive action.
func ReadProcessIdentity(int) (ProcessIdentity, bool) {
	return "", false
}
