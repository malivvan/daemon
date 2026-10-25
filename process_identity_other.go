//go:build !linux

package daemon

import "strconv"

func processIdentityCompatible(identity ProcessIdentity) bool {
	encoded := string(identity)
	value, err := strconv.ParseInt(encoded, 10, 64)
	return err == nil && value > 0 && strconv.FormatInt(value, 10) == encoded
}

// processIdentityScope is ProcessScopeLocal for a readable identity whatever pid:
// non-Linux identities carry no boot ID or namespace.
func processIdentityScope(_ int, identity ProcessIdentity) ProcessScope {
	if !processIdentityCompatible(identity) {
		return ProcessScopeUnknown
	}
	return ProcessScopeLocal
}

func runtimeProcessIdentities(pid int) (ProcessIdentity, ProcessIdentity) {
	identity, _ := ReadProcessIdentity(pid)
	return identity, ""
}
