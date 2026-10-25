package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CompatibleFunc returns true when a discovered daemon can serve this client.
type CompatibleFunc func(RuntimeRecord, PingInfo) bool

// FindFunc returns a live compatible daemon using caller-defined discovery.
// It must honor ctx so Manager.Ensure can enforce its timeout. Callbacks that
// use bearer credentials must prove the runtime endpoint with Proof.Probe
// before sending them; ordinary authenticated discovery can use Discover.Proof.
type FindFunc func(context.Context) (RuntimeRecord, PingInfo, bool, error)

// StartFunc starts a daemon in the background.
type StartFunc func(context.Context) error

// Manager coordinates discovery and optional auto-start. When FindFunc is nil,
// Find uses Store, Discover, and Compatible. Store is still required by Ensure
// because it identifies the start lock, even when FindFunc supplies discovery.
type Manager struct {
	Store      RuntimeStore
	Discover   DiscoverOptions
	Compatible CompatibleFunc
	FindFunc   FindFunc
	Start      StartFunc
}

// Find returns a live compatible daemon, when one is already running. When
// FindFunc is set, Find delegates discovery to it.
func (m Manager) Find(ctx context.Context) (RuntimeRecord, PingInfo, bool, error) {
	if m.FindFunc != nil {
		return m.FindFunc(ctx)
	}
	opts := m.Discover
	accept := opts.Accept
	opts.Accept = func(rec RuntimeRecord, info PingInfo) bool {
		if accept != nil && !accept(rec, info) {
			return false
		}
		return m.Compatible == nil || m.Compatible(rec, info)
	}
	return Discover(ctx, m.Store, opts)
}

// Ensure returns a live compatible daemon, starting one when necessary.
func (m Manager) Ensure(ctx context.Context, timeout time.Duration) (RuntimeRecord, PingInfo, error) {
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	deadline := time.Now().Add(timeout)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	if rec, info, ok, err := m.Find(ctx); err != nil || ok {
		return rec, info, err
	}
	if m.Start == nil {
		return RuntimeRecord{}, PingInfo{}, errors.New("daemon not running")
	}
	unlock, err := m.lockStart(ctx)
	if err != nil {
		return RuntimeRecord{}, PingInfo{}, err
	}
	defer unlock()

	if rec, info, ok, err := m.Find(ctx); err != nil || ok {
		return rec, info, err
	}
	if err := m.Start(ctx); err != nil {
		return RuntimeRecord{}, PingInfo{}, err
	}
	var lastErr error
	for time.Now().Before(deadline) {
		rec, info, ok, err := m.Find(ctx)
		if err != nil {
			lastErr = err
		} else if ok {
			return rec, info, nil
		}
		select {
		case <-ctx.Done():
			return RuntimeRecord{}, PingInfo{}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if lastErr != nil {
		return RuntimeRecord{}, PingInfo{}, fmt.Errorf("daemon failed to start within %s: %w", timeout, lastErr)
	}
	return RuntimeRecord{}, PingInfo{}, fmt.Errorf("daemon failed to start within %s", timeout)
}

func (m Manager) lockStart(ctx context.Context) (func(), error) {
	return m.Store.AcquireStartLock(ctx)
}

// StartDetachedOptions configures StartDetached.
type StartDetachedOptions struct {
	Executable      string
	Args            []string
	Dir             string
	Env             []string
	Stdout          io.Writer
	Stderr          io.Writer
	RefuseEphemeral bool
	AfterStart      func(*exec.Cmd)
	// Exited, when set, receives the child's exit result once the child
	// exits. A caller waiting for the daemon to become reachable uses it to
	// stop waiting for a child that has already died.
	Exited func(error)
}

// StartDetached starts a child process detached from the caller's process
// group where the platform supports it. On Windows the child starts without a
// console so terminal probes cannot block waiting for console input.
func StartDetached(ctx context.Context, opts StartDetachedOptions) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	exe := opts.Executable
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return err
		}
	}
	if opts.RefuseEphemeral && IsEphemeralExecutable(exe) {
		return fmt.Errorf("refusing to auto-start daemon from ephemeral binary %s", filepath.Base(exe))
	}
	cmd := exec.Command(exe, opts.Args...) //nolint:forbidigo,noctx // the detached daemon must outlive the caller's context
	cmd.Dir = opts.Dir
	if opts.Env != nil {
		cmd.Env = opts.Env
	}
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	detachChild(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start detached daemon: %w", err)
	}
	if opts.AfterStart != nil {
		opts.AfterStart(cmd)
	}
	go func() {
		err := cmd.Wait()
		if opts.Exited != nil {
			opts.Exited(err)
		}
	}()
	return nil
}

// IsEphemeralExecutable reports whether exe looks like a test or go-build
// binary that should not be used as a long-lived daemon.
func IsEphemeralExecutable(exe string) bool {
	base := filepath.Base(exe)
	lowerBase := strings.ToLower(base)
	if withoutExe, ok := strings.CutSuffix(lowerBase, ".exe"); ok {
		lowerBase = withoutExe
	}
	normalized := strings.ReplaceAll(exe, `\`, `/`)
	return strings.HasSuffix(lowerBase, ".test") || strings.Contains(normalized, "/go-build")
}

// ErrStopProofRequired means a TCP shutdown request needs endpoint proof.
var ErrStopProofRequired = errors.New("daemon stop requires endpoint proof")

// ErrExitUnconfirmed means the recorded process is gone but exit evidence is inconclusive.
var ErrExitUnconfirmed = errors.New("daemon exit is unconfirmed")

// StopOptions configures a shutdown request and the wait for daemon exit.
type StopOptions struct {
	Probe ProbeOptions
	// Proof is required for TCP records in every process scope.
	Proof *Proof
	// Request asks the verified daemon to shut down.
	// Its error ends Stop and is returned wrapped. Treat a connection dropped
	// during daemon shutdown as success so Stop can wait for exit.
	Request func(context.Context, RuntimeRecord) error
	// Exited supplies the caller's exit proof. When set, only a true result
	// confirms exit; a refused connection or another answering PID keeps waiting.
	// Without Exited, connection refusal ends the wait even if the process is
	// still draining. Set Exited when the caller needs to wait for process exit.
	Exited func(context.Context, RuntimeRecord) (bool, error)
	// Timeout bounds the whole call. Zero or negative values use 30 seconds.
	Timeout time.Duration
}

// Stop verifies rec's endpoint, requests shutdown, and waits for exit.
// TCP records require Proof in every process scope; Unix records may use a PID ping.
// When rec's process is proven gone, Stop checks exit evidence without a request.
// Without Exited, a stale Unix socket also confirms exit before any request.
// A record with no versioned identity is judged local, as in Discover.
// Without Exited, a refused connection or a replacement answering with a positive
// PID other than the recorded one ends the wait.
// Connection refusal doesn't prove process exit; the process may still be
// draining. Callers that need process exit supply Exited.
// A replacement answering with the recorded PID looks like the original, so
// the wait ends at its bound. Two PID namespaces can each have a daemon at PID 2.
// Callers that need to distinguish these replacements supply Exited.
// Stop sends no signals and removes no files.
func Stop(ctx context.Context, rec RuntimeRecord, opts StopOptions) error {
	if rec.PID <= 0 {
		return fmt.Errorf("daemon stop requires a positive PID: %d", rec.PID)
	}
	if opts.Request == nil {
		return errors.New("daemon stop request is required")
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ep := rec.Endpoint()
	if !runtimeRecordLive(rec) {
		exited, err := stopExitConfirmed(ctx, rec, opts)
		if err != nil {
			return err
		}
		if exited {
			return nil
		}
		return fmt.Errorf("stop daemon at %s: %w", ep.Address, ErrExitUnconfirmed)
	}
	if ep.Network != NetworkUnix && opts.Proof == nil {
		return stopErrorUnlessExited(ctx, rec, opts, fmt.Errorf("stop daemon at %s: %w", ep.Address, ErrStopProofRequired))
	}
	info, err := probeRecordEndpoint(ctx, rec, opts.Proof, opts.Probe)
	if err == nil && opts.Proof == nil && info.PID != rec.PID {
		err = fmt.Errorf("daemon ping returned PID %d", info.PID)
	}
	if err != nil {
		return stopErrorUnlessExited(ctx, rec, opts, &UnreachableError{Record: rec, Endpoint: ep, Err: err})
	}
	if err := opts.Request(ctx, rec); err != nil {
		return fmt.Errorf("request daemon shutdown: %w", err)
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for daemon exit: %w", err)
		}
		exited, err := stopExitConfirmed(ctx, rec, opts)
		if err != nil {
			return err
		}
		if exited {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for daemon exit: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func stopErrorUnlessExited(ctx context.Context, rec RuntimeRecord, opts StopOptions, stopErr error) error {
	if opts.Exited != nil {
		exited, err := stopExitConfirmed(ctx, rec, opts)
		if err != nil {
			return errors.Join(stopErr, err)
		}
		if exited {
			return nil
		}
	} else if rec.Endpoint().Network == NetworkUnix && isStaleUnixSocketDialError(stopErr) {
		return nil
	}
	return stopErr
}

func stopExitConfirmed(ctx context.Context, rec RuntimeRecord, opts StopOptions) (bool, error) {
	if opts.Exited != nil {
		exited, err := opts.Exited(ctx, rec)
		if err != nil {
			return false, fmt.Errorf("check daemon exit: %w", err)
		}
		return exited, nil
	}
	// A replacement with another service must also confirm exit.
	opts.Probe.ExpectedService = ""
	info, err := Probe(ctx, rec.Endpoint(), opts.Probe)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, fmt.Errorf("check daemon exit: %w", ctxErr)
	}
	return (err == nil && info.PID > 0 && info.PID != rec.PID) || isStaleUnixSocketDialError(err), nil
}
