package daemon

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// errNotPortable reports a file name that is not safe to use on every modern
// operating system and file system. Runtime prefix validation wraps it.
var errNotPortable = errors.New("name is not portable")

// maxPortableNameBytes is the portable component limit shared by common file
// systems.
const maxPortableNameBytes = 255

// reservedNames are the Windows device names that are reserved with any
// extension.
var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"CONIN$": true, "CONOUT$": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// checkPortableName reports an error wrapping errNotPortable unless name is a
// single file name that another OS and file system cannot misread. It rejects
// empty names, "." and "..", separators, characters invalid on any modern OS,
// invalid UTF-8, reserved device names with or without an extension, leading
// or trailing spaces, trailing dots, and names longer than 255 bytes.
//
// The name is read in every spelling a runtime prefix can take, because it
// becomes part of every runtime file name.
func checkPortableName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: name is empty", errNotPortable)
	case name == "." || name == "..":
		return fmt.Errorf("%w: %q is not a name", errNotPortable, name)
	case strings.ContainsAny(name, `/\`):
		return fmt.Errorf("%w: %q contains a path separator", errNotPortable, name)
	case !utf8.ValidString(name):
		return fmt.Errorf("%w: %q is not valid UTF-8", errNotPortable, name)
	case len(name) > maxPortableNameBytes:
		return fmt.Errorf("%w: %q is longer than %d bytes", errNotPortable, name, maxPortableNameBytes)
	case strings.HasSuffix(name, "."):
		return fmt.Errorf("%w: %q ends with a dot", errNotPortable, name)
	case strings.HasPrefix(name, " "):
		return fmt.Errorf("%w: %q starts with a space", errNotPortable, name)
	case strings.HasSuffix(name, " "):
		return fmt.Errorf("%w: %q ends with a space", errNotPortable, name)
	}
	for _, r := range name {
		// Windows rejects these characters, and control characters are not
		// representable on every file system.
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"|?*`, r) {
			return fmt.Errorf("%w: %q contains %q", errNotPortable, name, r)
		}
	}
	stem := name
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	// Windows ignores trailing spaces when it matches device names.
	if reservedNames[strings.ToUpper(strings.TrimRight(stem, " "))] {
		return fmt.Errorf("%w: %q is a reserved device name", errNotPortable, name)
	}
	return nil
}
