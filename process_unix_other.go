//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package daemon

import (
	"math"
	"runtime"
	"strconv"
	"syscall"
	"unsafe"
)

// processIdentityScope is ProcessScopeLocal for a readable identity whatever
// pid: non-Linux identities carry no boot ID or namespace.
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

// Darwin's kern.proc.pid sysctl returns a kinfo_proc whose first field is the
// process start time. Reading only that prefix keeps the package free of the
// full platform-specific struct.
const (
	darwinCTLKern     = 1
	darwinKernProc    = 14
	darwinKernProcPID = 1
	darwinSYSSysctl   = 202 // SYS___SYSCTL

	darwinKinfoProcSize = 0x288
)

// darwinTimeval is the start of kinfo_proc: P_starttime is its first field, so
// it sits at offset 0.
type darwinTimeval struct {
	Sec  int64
	Usec int32
	_    [4]byte
}

// ReadProcessIdentity returns pid's OS-reported creation time as an opaque
// identity. Only Darwin can inspect it on this build; every other platform
// keeps the identity unknown, which never authorizes destructive action.
func ReadProcessIdentity(pid int) (ProcessIdentity, bool) {
	if runtime.GOOS != "darwin" {
		return "", false
	}
	return readDarwinProcessIdentity(pid)
}

func readDarwinProcessIdentity(pid int) (ProcessIdentity, bool) {
	if pid <= 0 || pid > math.MaxInt32 {
		return "", false
	}
	mib := [4]int32{darwinCTLKern, darwinKernProc, darwinKernProcPID, int32(pid)}
	var info [darwinKinfoProcSize]byte
	size := uintptr(len(info))
	_, _, errno := syscall.Syscall6(
		darwinSYSSysctl,
		uintptr(unsafe.Pointer(&mib[0])),
		uintptr(len(mib)),
		uintptr(unsafe.Pointer(&info[0])),
		uintptr(unsafe.Pointer(&size)),
		0,
		0,
	)
	if errno != 0 || size != uintptr(len(info)) {
		return "", false
	}
	start := (*darwinTimeval)(unsafe.Pointer(&info[0]))
	millis := start.Sec*1000 + int64(start.Usec)/1000
	if millis <= 0 {
		return "", false
	}
	return ProcessIdentity(strconv.FormatInt(millis, 10)), true
}
