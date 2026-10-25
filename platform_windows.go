//go:build windows

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"unsafe"
)

// windowsDetachedProcess (DETACHED_PROCESS) starts the daemon without a console.
// Hidden consoles expose CONIN$, which can make terminal-probing libraries block
// forever when nothing is present to answer terminal queries.
const windowsDetachedProcess = 0x00000008

func detachChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= syscall.CREATE_NEW_PROCESS_GROUP | windowsDetachedProcess
}

// ProcessAlive reports whether pid appears to name a live process.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const processQueryLimitedInformation = 0x1000
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return errors.Is(err, syscall.ERROR_ACCESS_DENIED)
	}
	defer func() { _ = syscall.CloseHandle(handle) }()

	// A terminated process object remains openable while another handle keeps
	// it alive. The exit status, not the handle alone, distinguishes that state.
	const stillActive = 259
	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		return true
	}
	return exitCode == stillActive
}

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

// publishFile renames staging over target with MoveFileEx, which replaces an
// existing file and writes the rename through. It never copies, so a rename
// across volumes fails.
func publishFile(staging, target string) error {
	return moveFileEx(staging, target, moveFileReplaceExisting|moveFileWriteThrough)
}

// syncDir is a no-op on Windows: a directory handle cannot be fsynced to force
// entry changes to disk, and renames are write-through.
func syncDir(string) error { return nil }

// errorLockViolation is returned by LockFileEx when the lock is held and
// LOCKFILE_FAIL_IMMEDIATELY is set.
const errorLockViolation syscall.Errno = 33

// lockFilePlatform takes an exclusive LockFileEx lock on file. With
// nonblocking it reports false instead of waiting when another holder has the
// lock.
func lockFilePlatform(file *os.File, nonblocking bool) (bool, error) {
	err := lockFileExclusive(syscall.Handle(file.Fd()), nonblocking)
	if err == nil {
		return true, nil
	}
	if nonblocking && (errors.Is(err, errorLockViolation) || errors.Is(err, syscall.ERROR_IO_PENDING)) {
		return false, nil
	}
	return false, err
}

func unlockFilePlatform(file *os.File) error {
	return unlockFileEx(syscall.Handle(file.Fd()))
}

// wsaECONNREFUSED is WSAECONNREFUSED, which Windows reports both for a socket
// file with no listener and for a missing path; neither maps to
// syscall.ECONNREFUSED.
const wsaECONNREFUSED syscall.Errno = 10061

func isStaleUnixSocketDialError(err error) bool {
	return errors.Is(err, wsaECONNREFUSED)
}

// ensurePrivateRuntimeDir creates path when needed and verifies it is a
// non-reparse directory owned by the current token user, token owner,
// LocalSystem, or built-in Administrators with a DACL restricted to those
// trusted principals. It rewrites the DACL on every call to repair inherited
// child permissions.
func ensurePrivateRuntimeDir(path string) error {
	if path == "" {
		return errors.New("path is empty")
	}
	if err := rejectWindowsReparsePoint(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if err := rejectWindowsReparsePoint(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	handle, err := openWindowsDir(path)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.CloseHandle(handle) }()
	userSID, ownerSID, err := currentWindowsTokenSIDs()
	if err != nil {
		return err
	}
	if err := verifyWindowsDirHandle(path, handle, userSID, ownerSID); err != nil {
		return err
	}
	acl, err := privateWindowsACL(userSID)
	if err != nil {
		return err
	}
	return setDACL(handle, acl, daclSecurityInformation|protectedDACLSecurityInformation)
}

// validatePrivateRuntimeDir verifies path is a non-reparse directory owned by
// the trusted principals with a DACL restricted to them. It never creates or
// changes the directory.
func validatePrivateRuntimeDir(path string) error {
	if path == "" {
		return errors.New("path is empty")
	}
	if err := rejectWindowsReparsePoint(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", path)
	}
	handle, err := openWindowsDir(path)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.CloseHandle(handle) }()
	userSID, ownerSID, err := currentWindowsTokenSIDs()
	if err != nil {
		return err
	}
	if err := verifyWindowsDirHandle(path, handle, userSID, ownerSID); err != nil {
		return err
	}
	return verifyWindowsDACL(path, handle, userSID, ownerSID)
}

// openRuntimeFile opens path without following reparse points and verifies the
// opened handle is a regular file owned by the current token user or token
// owner.
func openRuntimeFile(path string) (*os.File, error) {
	if path == "" {
		return nil, errors.New("path is empty")
	}
	pathPtr, err := winPathPtr(path)
	if err != nil {
		return nil, err
	}
	handle, err := syscall.CreateFile(
		pathPtr,
		genericRead|readControl,
		fileShareRead|fileShareWrite|fileShareDelete,
		nil,
		openExisting,
		fileFlagOpenReparsePoint,
		0,
	)
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			_ = syscall.CloseHandle(handle)
		}
	}()
	if err := validateWindowsFileHandle(path, handle); err != nil {
		return nil, err
	}
	success = true
	return os.NewFile(uintptr(handle), path), nil
}

// runtimeUID returns a stable filesystem-safe identifier for the current
// Windows account.
func runtimeUID() string {
	sid, err := currentTokenSID(tokenUser)
	if err != nil {
		return "unknown"
	}
	str, err := sidString(sid)
	if err != nil {
		return "unknown"
	}
	sum := sha256.Sum256([]byte(str))
	return "sid-" + hex.EncodeToString(sum[:8])
}

// processQueryLimitedInformation opens a process with the least access that
// still allows its creation time to be read.
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

// processIdentityScope is ProcessScopeLocal for a readable identity whatever
// pid: Windows identities carry no boot ID or namespace.
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

func currentWindowsTokenSIDs() (userSID, ownerSID *winSID, err error) {
	userSID, err = currentTokenSID(tokenUser)
	if err != nil {
		return nil, nil, err
	}
	ownerSID, err = currentTokenSID(tokenOwner)
	if err != nil {
		return nil, nil, err
	}
	return userSID, ownerSID, nil
}

func rejectWindowsReparsePoint(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	pathPtr, err := winPathPtr(path)
	if err != nil {
		return err
	}
	attrs, err := syscall.GetFileAttributes(pathPtr)
	if err != nil {
		return err
	}
	if attrs&fileAttributeReparsePoint != 0 {
		return fmt.Errorf("%s is a reparse point", path)
	}
	return nil
}

func openWindowsDir(path string) (syscall.Handle, error) {
	pathPtr, err := winPathPtr(path)
	if err != nil {
		return 0, err
	}
	return syscall.CreateFile(
		pathPtr,
		readControl|writeDAC,
		fileShareRead|fileShareWrite|fileShareDelete,
		nil,
		openExisting,
		fileFlagBackupSemantics|fileFlagOpenReparsePoint,
		0,
	)
}

func verifyWindowsDirHandle(path string, handle syscall.Handle, userSID, ownerSID *winSID) error {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&fileAttributeReparsePoint != 0 {
		return fmt.Errorf("%s is a reparse point", path)
	}
	if info.FileAttributes&fileAttributeDirectory == 0 {
		return fmt.Errorf("%s is not a directory", path)
	}
	sd, err := securityDescriptor(handle, ownerSecurityInformation)
	if err != nil {
		return err
	}
	owner, err := securityDescriptorOwner(sd)
	if err != nil {
		return err
	}
	return verifyWindowsDirectoryOwner(path, owner, userSID, ownerSID)
}

func trustedWindowsDirectoryOwners(userSID, ownerSID *winSID) ([]*winSID, error) {
	systemSID, err := createWellKnownSID(winLocalSystemSid)
	if err != nil {
		return nil, err
	}
	adminsSID, err := createWellKnownSID(winBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	return []*winSID{userSID, ownerSID, systemSID, adminsSID}, nil
}

func verifyWindowsDirectoryOwner(path string, owner, userSID, ownerSID *winSID) error {
	trusted, err := trustedWindowsDirectoryOwners(userSID, ownerSID)
	if err != nil {
		return err
	}
	if !windowsAnyOwnerMatches(owner, trusted) {
		return fmt.Errorf("%s is not owned by current user, token owner, LocalSystem, or built-in Administrators", path)
	}
	return nil
}

func verifyWindowsDACL(path string, handle syscall.Handle, userSID, ownerSID *winSID) error {
	sd, err := securityDescriptor(handle, daclSecurityInformation)
	if err != nil {
		return err
	}
	control, err := securityDescriptorControl(sd)
	if err != nil {
		return err
	}
	if control&seDACLProtected == 0 {
		return fmt.Errorf("%s DACL is not protected", path)
	}
	if sd == nil {
		return fmt.Errorf("%s DACL is empty", path)
	}
	dacl, err := securityDescriptorDACL(sd)
	if err != nil {
		return err
	}
	if dacl == nil || dacl.AceCount == 0 {
		return fmt.Errorf("%s DACL is empty", path)
	}
	allowed, err := trustedWindowsDirectoryOwners(userSID, ownerSID)
	if err != nil {
		return err
	}
	for i := range uint32(dacl.AceCount) {
		ace, err := getACE(dacl, i)
		if err != nil {
			return err
		}
		if ace.Header.AceType != accessAllowedACEType {
			return fmt.Errorf("%s DACL contains non-allow ACE", path)
		}
		sid := (*winSID)(unsafe.Pointer(&ace.SidStart))
		if !windowsAnyOwnerMatches(sid, allowed) {
			return fmt.Errorf("%s DACL grants access to unexpected principal", path)
		}
	}
	return nil
}

func windowsAnyOwnerMatches(owner *winSID, allowed []*winSID) bool {
	for _, sid := range allowed {
		if equalSID(owner, sid) {
			return true
		}
	}
	return false
}

// privateWindowsACL grants full control to the current user, LocalSystem, and
// built-in Administrators, with inheritance for child entries.
func privateWindowsACL(userSID *winSID) (*winACL, error) {
	systemSID, err := createWellKnownSID(winLocalSystemSid)
	if err != nil {
		return nil, err
	}
	adminsSID, err := createWellKnownSID(winBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	entries := []winExplicitAccess{
		allowFullControl(userSID, trusteeIsUser),
	}
	if !equalSID(userSID, systemSID) {
		entries = append(entries, allowFullControl(systemSID, trusteeIsUser))
	}
	if !equalSID(userSID, adminsSID) {
		entries = append(entries, allowFullControl(adminsSID, trusteeIsGroup))
	}
	return aclFromEntries(entries)
}

func allowFullControl(sid *winSID, trusteeType uint32) winExplicitAccess {
	// FILE_ALL_ACCESS (0x1F01FF) avoids Windows splitting generic inheritable
	// ACEs.
	const fileAllAccess = 0x1F01FF
	return winExplicitAccess{
		AccessPermissions: fileAllAccess,
		AccessMode:        grantAccess,
		Inheritance:       subContainersAndObjectsInherit,
		Trustee: winTrustee{
			TrusteeForm:  trusteeIsSID,
			TrusteeType:  trusteeType,
			TrusteeValue: uintptr(unsafe.Pointer(sid)),
		},
	}
}

func validateWindowsFileHandle(path string, handle syscall.Handle) error {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &info); err != nil {
		return err
	}
	if info.FileAttributes&fileAttributeReparsePoint != 0 {
		return fmt.Errorf("%s is a reparse point", path)
	}
	if info.FileAttributes&fileAttributeDirectory != 0 {
		return fmt.Errorf("%s is a directory", path)
	}
	sd, err := securityDescriptor(handle, ownerSecurityInformation)
	if err != nil {
		return err
	}
	owner, err := securityDescriptorOwner(sd)
	if err != nil {
		return err
	}
	userSID, err := currentTokenSID(tokenUser)
	if err != nil {
		return err
	}
	ownerSID, err := currentTokenSID(tokenOwner)
	if err != nil {
		return err
	}
	if !equalSID(owner, userSID) && !equalSID(owner, ownerSID) {
		return fmt.Errorf("%s is not owned by current user or token owner", path)
	}
	return nil
}
