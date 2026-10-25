//go:build windows

package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

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
