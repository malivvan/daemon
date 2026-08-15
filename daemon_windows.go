//go:build windows

package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

var (
	modkernel32                   = syscall.NewLazyDLL("kernel32.dll")
	procExpandEnvironmentStringsW = modkernel32.NewProc("ExpandEnvironmentStringsW")
)

// Start starts a new process specified by cmdPath and cmdArgs in a detached state on Windows.
// The new process will not be attached to the current console and will run independently.
func Start(args []string) (int, error) {
	envMap := make(map[string]string)

	// Obtain COMPUTERNAME, SYSTEMDRIVE, USERPROFILE, etc. from current environment
	for _, env := range os.Environ() {
		if strings.HasPrefix(env, "=") {
			continue // Skip entries starting with "="
		}
		parts := strings.SplitN(env, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	// Read SYSTEM environment vars
	sysReg, err := registry.OpenKey(
		registry.LOCAL_MACHINE,
		`SYSTEM\CurrentControlSet\Control\Session Manager\Environment`,
		registry.READ,
	)
	if err == nil {
		defer func() {
			sysReg.Close()     //nolint:errcheck
			_ = sysReg.Close() //nolint:errcheck
		}()
		sysEnv, _ := sysReg.ReadValueNames(0)
		for _, name := range sysEnv {
			val, _, _ := sysReg.GetStringValue(name)
			envMap[name] = val
		}
	}

	// Read USER environment vars
	userReg, err := registry.OpenKey(
		registry.CURRENT_USER,
		`Environment`,
		registry.READ,
	)
	if err == nil {
		defer func() {
			_ = userReg.Close() //nolint:errcheck
		}()
		userEnv, _ := userReg.ReadValueNames(0)
		for _, name := range userEnv {
			val, _, _ := userReg.GetStringValue(name)
			if name == "Path" || name == "PsModulePath" {
				// Append USER Path to SYSTEM Path (system first, then user)
				envMap[name] = envMap[name] + ";" + val
			} else {
				envMap[name] = val
			}
		}
	}

	// Construct env []string in "key=value" format, expanding variables as we go
	env := make([]string, 0, len(envMap))
	for k, v := range envMap {
		src, err := syscall.UTF16PtrFromString(v)
		if err != nil {
			return -1, fmt.Errorf("string with NULL passed to StringToUTF16Ptr")
		}
		buf := make([]uint16, 32767) // Maximum environment variable size on Windows
		dst := &buf[0]
		size := uintptr(len(buf))

		n, _, _ := procExpandEnvironmentStringsW.Call(
			uintptr(unsafe.Pointer(src)),
			uintptr(unsafe.Pointer(dst)),
			size,
		)
		if n != 0 && n <= size {
			v = syscall.UTF16ToString(buf[:n-1])
		}

		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	cmd := exec.Command(_path, args...)
	cmd.SysProcAttr = &windows.SysProcAttr{
		NoInheritHandles: true,
		CreationFlags:    windows.CREATE_NEW_PROCESS_GROUP, // | windows.DETACHED_PROCESS,
	}
	cmd.Env = env

	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.Stdin = nil

	err = cmd.Start()
	if err != nil {
		return -1, fmt.Errorf("failed to start detached process: %w", err)
	}

	err = cmd.Process.Release()
	if err != nil {
		return -1, fmt.Errorf("failed to release detached process: %w", err)
	}

	return cmd.Process.Pid, nil
}

func Stop(pid int) error {
	d, e := syscall.LoadDLL("kernel32.dll")
	if e != nil {
		return e
	}
	p, e := d.FindProc("GenerateConsoleCtrlEvent")
	if e != nil {
		return e
	}
	r, _, e := p.Call(uintptr(syscall.CTRL_BREAK_EVENT), uintptr(pid))
	if r == 0 {
		return e // syscall.GetLastError()
	}
	return nil
}

// Kill terminates a process with the given PID on windows.
func Kill(pid int) error {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(handle) }()
	err = windows.TerminateProcess(handle, 0)
	if err != nil {
		return err
	}
	return nil
}
