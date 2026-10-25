package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// RuntimeRecord is the on-disk daemon.<pid>.json shape used for discovery.
type RuntimeRecord struct {
	PID               int               `json:"pid"`
	ProcessIdentity   ProcessIdentity   `json:"process_identity,omitempty"`
	ProcessIdentityV2 ProcessIdentity   `json:"process_identity_v2,omitempty"`
	Network           string            `json:"network,omitempty"`
	Address           string            `json:"address"`
	Service           string            `json:"service,omitempty"`
	Version           string            `json:"version,omitempty"`
	StartedAt         time.Time         `json:"started_at,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`

	// SourcePath is set by List and ignored on disk.
	SourcePath string `json:"-"`
}

// NewRuntimeRecord returns a record for the current process.
func NewRuntimeRecord(service, version string, ep Endpoint) RuntimeRecord {
	pid := os.Getpid()
	legacyIdentity, identityV2 := runtimeProcessIdentities(pid)
	return RuntimeRecord{
		PID:               pid,
		ProcessIdentity:   legacyIdentity,
		ProcessIdentityV2: identityV2,
		Network:           ep.Network,
		Address:           ep.Address,
		Service:           service,
		Version:           version,
		StartedAt:         time.Now().UTC(),
	}
}

// Endpoint returns the daemon endpoint described by r.
func (r RuntimeRecord) Endpoint() Endpoint {
	network := r.Network
	address := r.Address
	if path, ok := strings.CutPrefix(address, "unix://"); ok {
		network = NetworkUnix
		address = path
	}
	if network == "" {
		network = NetworkTCP
	}
	return Endpoint{Network: network, Address: address}
}

// RuntimeStore owns daemon runtime files in a directory.
type RuntimeStore struct {
	Dir    string
	Prefix string
}

const (
	runtimeWriteCheckPattern = ".%s.write-check-*"
	// maxRuntimePrefixBytes leaves room in the portable 255-byte component
	// limit for .<prefix>.<19-digit PID>.json.tmp-<10-digit random value>.
	maxRuntimePrefixBytes = 214
)

func (s RuntimeStore) prefix() string {
	if s.Prefix == "" {
		return "daemon"
	}
	return s.Prefix
}

func (s RuntimeStore) validatePrefix() (string, error) {
	prefix := s.prefix()
	// The prefix becomes part of every runtime file name, so it must be a
	// name that works on every OS: "a:b" would name an NTFS stream and
	// "NUL" a device on Windows.
	if err := checkPortableName(prefix); err != nil {
		return "", fmt.Errorf("runtime prefix %q must be a portable basename: %w", prefix, err)
	}
	if len(prefix) > maxRuntimePrefixBytes {
		return "", fmt.Errorf(
			"runtime prefix %q exceeds %d bytes: %w",
			prefix, maxRuntimePrefixBytes, errNotPortable,
		)
	}
	return prefix, nil
}

func (s RuntimeStore) prepareDir() error {
	if s.Dir == "" {
		return errors.New("runtime dir is empty")
	}
	if !filepath.IsAbs(s.Dir) {
		return fmt.Errorf("runtime dir %q must be absolute", s.Dir)
	}
	if err := ensurePrivateRuntimeDir(s.Dir); err != nil {
		return fmt.Errorf("prepare runtime dir: %w", err)
	}
	return nil
}

// CheckWritable verifies that the calling process can create and remove a file
// in the runtime directory. It is an advisory preflight: it does not guarantee
// future access or that a child process has the same permissions.
func (s RuntimeStore) CheckWritable() error {
	prefix, err := s.validatePrefix()
	if err != nil {
		return err
	}
	if err := s.prepareDir(); err != nil {
		return err
	}
	probe, err := os.CreateTemp(s.Dir, fmt.Sprintf(runtimeWriteCheckPattern, prefix))
	if err != nil {
		return fmt.Errorf("create runtime write check: %w", err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probePath)
		return fmt.Errorf("close runtime write check: %w", err)
	}
	if err := os.Remove(probePath); err != nil {
		return fmt.Errorf("remove runtime write check: %w", err)
	}
	return nil
}

// Path returns the runtime file path for pid.
func (s RuntimeStore) Path(pid int) (string, error) {
	prefix, err := s.validatePrefix()
	if err != nil {
		return "", err
	}
	if pid <= 0 {
		return "", errors.New("pid must be > 0")
	}
	return filepath.Join(s.Dir, fmt.Sprintf("%s.%d.json", prefix, pid)), nil
}

// LockPath returns the path used to serialize daemon auto-starts for the store.
func (s RuntimeStore) LockPath() (string, error) {
	prefix, err := s.validatePrefix()
	if err != nil {
		return "", err
	}
	if err := s.prepareDir(); err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, prefix+".lock"), nil
}

// ListenLockPath returns the path used to serialize daemon server listen setup
// for the store.
func (s RuntimeStore) ListenLockPath() (string, error) {
	prefix, err := s.validatePrefix()
	if err != nil {
		return "", err
	}
	if err := s.prepareDir(); err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, prefix+".listen.lock"), nil
}

// Write saves rec atomically and returns the final path.
func (s RuntimeStore) Write(rec RuntimeRecord) (string, error) {
	if err := s.prepareDir(); err != nil {
		return "", err
	}
	if rec.PID <= 0 {
		return "", errors.New("pid must be > 0")
	}
	if rec.Address == "" {
		return "", errors.New("runtime address is empty")
	}
	if rec.StartedAt.IsZero() {
		rec.StartedAt = time.Now().UTC()
	}
	final, err := s.Path(rec.PID)
	if err != nil {
		return "", err
	}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal runtime record: %w", err)
	}
	if err := writeAtomicFile(final, body, 0o644); err != nil {
		if errors.Is(err, errPublished) {
			return final, fmt.Errorf("write runtime file: %w", err)
		}
		return "", fmt.Errorf("write runtime file: %w", err)
	}
	return final, nil
}

// Read parses one runtime file.
func (s RuntimeStore) Read(path string) (RuntimeRecord, error) {
	if err := s.prepareDir(); err != nil {
		return RuntimeRecord{}, err
	}
	return s.readPrepared(path)
}

func (s RuntimeStore) readPrepared(path string) (RuntimeRecord, error) {
	file, err := openRuntimeFile(path)
	if err != nil {
		return RuntimeRecord{}, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(file)
	if err != nil {
		return RuntimeRecord{}, fmt.Errorf("read runtime file %s: %w", path, err)
	}
	var rec RuntimeRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return RuntimeRecord{}, fmt.Errorf("parse runtime file %s: %w", path, err)
	}
	rec.SourcePath = path
	return rec, nil
}

// List returns valid runtime records in deterministic order. Malformed files
// and temp files are ignored.
func (s RuntimeStore) List() ([]RuntimeRecord, error) {
	prefix, err := s.validatePrefix()
	if err != nil {
		return nil, err
	}
	if err := s.prepareDir(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runtime dir: %w", err)
	}
	var records []RuntimeRecord
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		filenamePID, ok := pidFromName(prefix, name)
		if !ok {
			continue
		}
		path := filepath.Join(s.Dir, name)
		rec, err := s.readPrepared(path)
		if err != nil || rec.PID != filenamePID || rec.Address == "" {
			continue
		}
		records = append(records, rec)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].StartedAt.Equal(records[j].StartedAt) {
			return records[i].PID < records[j].PID
		}
		return records[i].StartedAt.Before(records[j].StartedAt)
	})
	return records, nil
}

// CleanupDead removes runtime files whose filename PID matches the record PID
// and whose process is no longer alive (see RuntimeProcessAlive). Records this
// caller can't judge (see AGENTS.md), malformed files, and PID-mismatched files
// are left in place for human inspection.
func (s RuntimeStore) CleanupDead() (int, error) {
	prefix, err := s.validatePrefix()
	if err != nil {
		return 0, err
	}
	if err := s.prepareDir(); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("read runtime dir: %w", err)
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		filenamePID, ok := pidFromName(prefix, name)
		if !ok {
			continue
		}
		path := filepath.Join(s.Dir, name)
		rec, err := s.readPrepared(path)
		if err != nil || rec.PID != filenamePID || RuntimeProcessAlive(rec) {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	return removed, nil
}

func pidFromName(prefix, name string) (int, bool) {
	filePrefix := prefix + "."
	if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	mid := strings.TrimSuffix(strings.TrimPrefix(name, filePrefix), ".json")
	pid, err := strconv.Atoi(mid)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

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

// errPublished marks a failure after the new content became visible at the
// target. Callers must not retry blindly: a retry rewrites content that is
// already visible.
var errPublished = errors.New("atomic write: published")

// errNotDurable marks a post-publication directory sync failure: the target
// already holds the new content, but a crash may still lose it. It wraps
// errPublished.
var errNotDurable = fmt.Errorf("%w but not confirmed durable", errPublished)

// writeAtomicFile atomically replaces the file at path with data. It stages
// data in a temporary file next to the target, fsyncs and closes it, renames
// it over path, and fsyncs path's directory. Readers see the old or the new
// content, never a partial file. Parent directories are not created.
//
// The staging name is ".<target>.tmp-*", so it stays outside the
// "<prefix>.<pid>.json" namespace that RuntimeStore matches.
//
// An error wrapping errPublished means the new content is already visible at
// path; see errPublished and errNotDurable.
var writeAtomicFile = func(path string, data []byte, perm os.FileMode) error {
	staged, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("atomic write %s: %w", path, err)
	}
	stagedPath := staged.Name()
	// A successful publish removes the staging name; any failure before that
	// must not leave it behind.
	published := false
	defer func() {
		if published {
			return
		}
		_ = staged.Close()
		_ = os.Remove(stagedPath)
	}()
	if err := staged.Chmod(perm); err != nil {
		return fmt.Errorf("atomic write %s: chmod staging file: %w", path, err)
	}
	if _, err := staged.Write(data); err != nil {
		return fmt.Errorf("atomic write %s: write staging file: %w", path, err)
	}
	if err := staged.Sync(); err != nil {
		return fmt.Errorf("atomic write %s: sync staging file: %w", path, err)
	}
	if err := staged.Close(); err != nil {
		return fmt.Errorf("atomic write %s: close staging file: %w", path, err)
	}
	if err := publishFile(stagedPath, path); err != nil {
		return fmt.Errorf("atomic write %s: %w", path, err)
	}
	published = true
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: %w", errNotDurable, err)
	}
	return nil
}
