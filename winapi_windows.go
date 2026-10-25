//go:build windows

package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// Win32 API bindings used by the package's Windows implementation. The
// standard library's syscall package covers most calls; the security APIs and
// the file-locking calls below are bound directly.

const (
	moveFileReplaceExisting = 0x1
	moveFileWriteThrough    = 0x8

	lockfileFailImmediately = 0x1
	lockfileExclusiveLock   = 0x2

	fileAttributeDirectory    = 0x00000010
	fileAttributeReparsePoint = 0x00000400

	readControl  = 0x00020000
	writeDAC     = 0x00040000
	genericRead  = 0x80000000
	deleteAccess = 0x00010000

	fileShareRead   = 0x1
	fileShareWrite  = 0x2
	fileShareDelete = 0x4

	openExisting = 3

	fileFlagBackupSemantics  = 0x02000000
	fileFlagOpenReparsePoint = 0x00200000

	seFileObject = 1

	ownerSecurityInformation         = 0x00000001
	daclSecurityInformation          = 0x00000004
	protectedDACLSecurityInformation = 0x80000000

	seDACLProtected = 0x1000

	accessAllowedACEType = 0

	trusteeIsSID   = 0
	trusteeIsUser  = 1
	trusteeIsGroup = 2
	grantAccess    = 1

	subContainersAndObjectsInherit = 0x3

	winLocalSystemSid           = 22
	winBuiltinAdministratorsSid = 26

	tokenUser  = 1
	tokenOwner = 4

	securityMaxSIDSize = 68
)

var (
	advapi32 = syscall.NewLazyDLL("advapi32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")

	procSetEntriesInAclW             = advapi32.NewProc("SetEntriesInAclW")
	procGetAce                       = advapi32.NewProc("GetAce")
	procGetSecurityInfo              = advapi32.NewProc("GetSecurityInfo")
	procSetSecurityInfo              = advapi32.NewProc("SetSecurityInfo")
	procCreateWellKnownSid           = advapi32.NewProc("CreateWellKnownSid")
	procEqualSid                     = advapi32.NewProc("EqualSid")
	procGetSecurityDescriptorControl = advapi32.NewProc("GetSecurityDescriptorControl")
	procGetSecurityDescriptorOwner   = advapi32.NewProc("GetSecurityDescriptorOwner")
	procGetSecurityDescriptorDacl    = advapi32.NewProc("GetSecurityDescriptorDacl")
	procGetSecurityDescriptorLength  = advapi32.NewProc("GetSecurityDescriptorLength")
	procMoveFileExW                  = kernel32.NewProc("MoveFileExW")
	procLockFileEx                   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx                 = kernel32.NewProc("UnlockFileEx")
)

// winSID is the standard library's security identifier type.
type winSID = syscall.SID

// winACL mirrors the Win32 ACL header.
type winACL struct {
	ACLRevision byte
	Sbz1        byte
	ACLSize     uint16
	AceCount    uint16
	Sbz2        uint16
}

// winSecurityDescriptor mirrors the Win32 self-relative security descriptor.
// Its owner, group, sacl and dacl fields are byte offsets from the start of the
// descriptor, not pointers, and must be read with the GetSecurityDescriptor*
// functions.
type winSecurityDescriptor struct {
	Revision byte
	Sbz1     byte
	Control  uint16
	Owner    *winSID
	Group    *winSID
	Sacl     *winACL
	Dacl     *winACL
}

type winACEHeader struct {
	AceType  uint8
	AceFlags uint8
	AceSize  uint16
}

type winAccessAllowedACE struct {
	Header   winACEHeader
	Mask     uint32
	SidStart uint32
}

type winTrustee struct {
	MultipleTrustee          *winTrustee
	MultipleTrusteeOperation uint32
	TrusteeForm              uint32
	TrusteeType              uint32
	TrusteeValue             uintptr
}

type winExplicitAccess struct {
	AccessPermissions uint32
	AccessMode        uint32
	Inheritance       uint32
	Trustee           winTrustee
}

type winSIDAndAttributes struct {
	Sid        *winSID
	Attributes uint32
}

type winTokenUser struct {
	User winSIDAndAttributes
}

type winTokenOwner struct {
	Owner *winSID
}

// winErr normalizes the error returned by LazyProc.Call, which is always a
// non-nil syscall.Errno, into nil on success.
func winErr(err error) error {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		if errno == 0 {
			return nil
		}
		return errno
	}
	return err
}

// winPathPtr returns an absolute UTF-16 path, using the \\?\ long-path form
// when the path approaches MAX_PATH.
func winPathPtr(path string) (*uint16, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if len(abs) >= 248 {
		if strings.HasPrefix(abs, `\\`) {
			abs = `\\?\UNC\` + strings.TrimPrefix(abs, `\\`)
		} else {
			abs = `\\?\` + abs
		}
	}
	return syscall.UTF16PtrFromString(abs)
}

func moveFileEx(from, to string, flags uint32) error {
	fromPtr, err := winPathPtr(from)
	if err != nil {
		return err
	}
	toPtr, err := winPathPtr(to)
	if err != nil {
		return err
	}
	r1, _, callErr := procMoveFileExW.Call(
		uintptr(unsafe.Pointer(fromPtr)),
		uintptr(unsafe.Pointer(toPtr)),
		uintptr(flags),
	)
	if r1 == 0 {
		return winErr(callErr)
	}
	return nil
}

// securityDescriptor retrieves the requested security information for handle
// and copies the returned self-relative descriptor onto the Go heap.
func securityDescriptor(handle syscall.Handle, info uint32) (*winSecurityDescriptor, error) {
	var heapSD *winSecurityDescriptor
	r0, _, _ := procGetSecurityInfo.Call(
		uintptr(handle),
		uintptr(seFileObject),
		uintptr(info),
		0, 0, 0, 0,
		uintptr(unsafe.Pointer(&heapSD)),
	)
	if r0 != 0 {
		return nil, syscall.Errno(r0)
	}
	if heapSD == nil {
		return nil, nil
	}
	defer func() { _, _ = syscall.LocalFree(syscall.Handle(unsafe.Pointer(heapSD))) }()
	size := securityDescriptorLength(heapSD)
	if size == 0 {
		return nil, errors.New("security descriptor is empty")
	}
	buf := make([]byte, size)
	copy(buf, unsafe.Slice((*byte)(unsafe.Pointer(heapSD)), size))
	return (*winSecurityDescriptor)(unsafe.Pointer(&buf[0])), nil
}

func securityDescriptorLength(sd *winSecurityDescriptor) uint32 {
	r0, _, _ := procGetSecurityDescriptorLength.Call(uintptr(unsafe.Pointer(sd)))
	return uint32(r0)
}

func securityDescriptorControl(sd *winSecurityDescriptor) (uint16, error) {
	var control uint16
	var revision uint32
	r1, _, callErr := procGetSecurityDescriptorControl.Call(
		uintptr(unsafe.Pointer(sd)),
		uintptr(unsafe.Pointer(&control)),
		uintptr(unsafe.Pointer(&revision)),
	)
	if r1 == 0 {
		return 0, winErr(callErr)
	}
	return control, nil
}

func securityDescriptorOwner(sd *winSecurityDescriptor) (*winSID, error) {
	var owner *winSID
	var defaulted int32
	r1, _, callErr := procGetSecurityDescriptorOwner.Call(
		uintptr(unsafe.Pointer(sd)),
		uintptr(unsafe.Pointer(&owner)),
		uintptr(unsafe.Pointer(&defaulted)),
	)
	if r1 == 0 {
		return nil, winErr(callErr)
	}
	if owner == nil {
		return nil, errors.New("security descriptor owner is missing")
	}
	return owner, nil
}

func securityDescriptorDACL(sd *winSecurityDescriptor) (*winACL, error) {
	var dacl *winACL
	var present, defaulted int32
	r1, _, callErr := procGetSecurityDescriptorDacl.Call(
		uintptr(unsafe.Pointer(sd)),
		uintptr(unsafe.Pointer(&present)),
		uintptr(unsafe.Pointer(&dacl)),
		uintptr(unsafe.Pointer(&defaulted)),
	)
	if r1 == 0 {
		return nil, winErr(callErr)
	}
	if present == 0 {
		return nil, nil
	}
	return dacl, nil
}

// aclFromEntries builds an ACL on the Go heap from explicit access entries.
func aclFromEntries(entries []winExplicitAccess) (*winACL, error) {
	var first *winExplicitAccess
	if len(entries) > 0 {
		first = &entries[0]
	}
	var heapACL *winACL
	r0, _, _ := procSetEntriesInAclW.Call(
		uintptr(len(entries)),
		uintptr(unsafe.Pointer(first)),
		0,
		uintptr(unsafe.Pointer(&heapACL)),
	)
	if r0 != 0 {
		return nil, syscall.Errno(r0)
	}
	if heapACL == nil {
		return nil, errors.New("ACL is missing")
	}
	defer func() { _, _ = syscall.LocalFree(syscall.Handle(unsafe.Pointer(heapACL))) }()
	buf := make([]byte, heapACL.ACLSize)
	copy(buf, unsafe.Slice((*byte)(unsafe.Pointer(heapACL)), len(buf)))
	return (*winACL)(unsafe.Pointer(&buf[0])), nil
}

func setDACL(handle syscall.Handle, acl *winACL, info uint32) error {
	r0, _, _ := procSetSecurityInfo.Call(
		uintptr(handle),
		uintptr(seFileObject),
		uintptr(info),
		0, 0,
		uintptr(unsafe.Pointer(acl)),
		0,
	)
	if r0 != 0 {
		return syscall.Errno(r0)
	}
	return nil
}

func getACE(acl *winACL, index uint32) (*winAccessAllowedACE, error) {
	var ace *winAccessAllowedACE
	r1, _, callErr := procGetAce.Call(
		uintptr(unsafe.Pointer(acl)),
		uintptr(index),
		uintptr(unsafe.Pointer(&ace)),
	)
	if r1 == 0 {
		return nil, winErr(callErr)
	}
	if ace == nil {
		return nil, errors.New("ACL entry is missing")
	}
	return ace, nil
}

func createWellKnownSID(sidType uint32) (*winSID, error) {
	buf := make([]byte, securityMaxSIDSize)
	size := uint32(len(buf))
	r1, _, callErr := procCreateWellKnownSid.Call(
		uintptr(sidType),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r1 == 0 {
		return nil, winErr(callErr)
	}
	return (*winSID)(unsafe.Pointer(&buf[0])), nil
}

func equalSID(a, b *winSID) bool {
	if a == nil || b == nil {
		return false
	}
	r1, _, _ := procEqualSid.Call(uintptr(unsafe.Pointer(a)), uintptr(unsafe.Pointer(b)))
	return r1 != 0
}

func copySID(sid *winSID) (*winSID, error) {
	n := syscall.GetLengthSid(sid)
	if n == 0 {
		return nil, errors.New("SID length is zero")
	}
	buf := make([]byte, n)
	if err := syscall.CopySid(n, (*winSID)(unsafe.Pointer(&buf[0])), sid); err != nil {
		return nil, err
	}
	return (*winSID)(unsafe.Pointer(&buf[0])), nil
}

func sidString(sid *winSID) (string, error) {
	var str *uint16
	if err := syscall.ConvertSidToStringSid(sid, &str); err != nil {
		return "", err
	}
	defer func() { _, _ = syscall.LocalFree(syscall.Handle(unsafe.Pointer(str))) }()
	// A SID string is at most 184 characters.
	buf := unsafe.Slice(str, 184)
	for i, c := range buf {
		if c == 0 {
			buf = buf[:i]
			break
		}
	}
	return syscall.UTF16ToString(buf), nil
}

// currentTokenSID returns the SID for the requested token information class
// (tokenUser or tokenOwner) of the current process.
func currentTokenSID(infoClass uint32) (*winSID, error) {
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return nil, err
	}
	var token syscall.Token
	if err := syscall.OpenProcessToken(process, syscall.TOKEN_QUERY, &token); err != nil {
		return nil, err
	}
	defer func() { _ = syscall.CloseHandle(syscall.Handle(token)) }()

	size := uint32(64)
	for {
		buf := make([]byte, size)
		err := syscall.GetTokenInformation(token, infoClass, &buf[0], uint32(len(buf)), &size)
		if err == nil {
			var sid *winSID
			switch infoClass {
			case tokenUser:
				sid = (*winTokenUser)(unsafe.Pointer(&buf[0])).User.Sid
			case tokenOwner:
				sid = (*winTokenOwner)(unsafe.Pointer(&buf[0])).Owner
			default:
				return nil, errors.New("unexpected token information class")
			}
			if sid == nil {
				return nil, errors.New("current token SID is missing")
			}
			return copySID(sid)
		}
		if !errors.Is(err, syscall.ERROR_INSUFFICIENT_BUFFER) || size <= uint32(len(buf)) {
			return nil, err
		}
	}
}

func lockFileExclusive(handle syscall.Handle, nonblocking bool) error {
	flags := uint32(lockfileExclusiveLock)
	if nonblocking {
		flags |= lockfileFailImmediately
	}
	var overlapped syscall.Overlapped
	r1, _, callErr := procLockFileEx.Call(
		uintptr(handle),
		uintptr(flags),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if r1 == 0 {
		return winErr(callErr)
	}
	return nil
}

func unlockFileEx(handle syscall.Handle) error {
	var overlapped syscall.Overlapped
	r1, _, callErr := procUnlockFileEx.Call(
		uintptr(handle),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&overlapped)),
	)
	if r1 == 0 {
		return winErr(callErr)
	}
	return nil
}
