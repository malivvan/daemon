package daemon_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/malivvan/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestKillHelper is not a real test: TestKillTerminatesProcess re-executes the
// test binary with -test.run targeting it so Kill has a live process to end.
func TestKillHelper(t *testing.T) {
	if os.Getenv("KIT_DAEMON_TEST_KILL_HELPER") == "" {
		t.Skip("helper process for TestKillTerminatesProcess")
	}
	time.Sleep(time.Minute)
}

func TestKillTerminatesProcess(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.Command(exe, "-test.run", "^TestKillHelper$")
	cmd.Env = append(os.Environ(), "KIT_DAEMON_TEST_KILL_HELPER=1")
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	pid := cmd.Process.Pid
	require.True(t, daemon.ProcessAlive(pid))
	require.NoError(t, daemon.Kill(pid))

	require.Error(t, cmd.Wait())
	require.False(t, daemon.ProcessAlive(pid))
}

func TestKillRejectsNonPositivePID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		require.Error(t, daemon.Kill(pid), "pid %d must be rejected before any signal is sent", pid)
	}
	// A zero PID names the caller's process group on Unix; the guard must keep
	// this process alive.
	require.True(t, daemon.ProcessAlive(os.Getpid()))
}

func TestProcessIdentityMatchesTheSameLiveProcess(t *testing.T) {
	identity, ok := daemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	require.NotEmpty(t, identity)
	assert.Equal(t, daemon.ProcessIdentityMatch,
		daemon.CompareProcessIdentity(os.Getpid(), identity))
}

func TestProcessIdentityRejectsAMismatchedIdentity(t *testing.T) {
	identity, ok := daemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	rec := mismatchedRuntimeIdentity(t, daemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: identity})
	assert.Equal(t, daemon.ProcessIdentityMismatch,
		daemon.CompareProcessIdentity(os.Getpid(), rec.ProcessIdentityV2))
}

func mismatchedRuntimeIdentity(t *testing.T, rec daemon.RuntimeRecord) daemon.RuntimeRecord {
	t.Helper()
	identity := string(rec.ProcessIdentityV2)
	if runtime.GOOS == "linux" {
		fields := linuxIdentityFields(t, rec)
		startTicks, err := strconv.ParseUint(fields[3], 10, 64)
		require.NoError(t, err)
		return withLinuxIdentityField(t, rec, 3, strconv.FormatUint(startTicks+1, 10))
	}
	created, err := strconv.ParseUint(identity, 10, 64)
	require.NoError(t, err)
	rec.ProcessIdentityV2 = daemon.ProcessIdentity(strconv.FormatUint(created+1, 10))
	return rec
}

func linuxIdentityFields(t *testing.T, rec daemon.RuntimeRecord) []string {
	t.Helper()
	fields := strings.Split(string(rec.ProcessIdentityV2), ":")
	require.Len(t, fields, 4)
	return fields
}

func withLinuxIdentityField(t *testing.T, rec daemon.RuntimeRecord, field int, value string) daemon.RuntimeRecord {
	t.Helper()
	fields := linuxIdentityFields(t, rec)
	fields[field] = value
	rec.ProcessIdentityV2 = daemon.ProcessIdentity(strings.Join(fields, ":"))
	return rec
}

func TestProcessIdentityTreatsMalformedIdentityAsUnknown(t *testing.T) {
	assert.Equal(t, daemon.ProcessIdentityUnknown,
		daemon.CompareProcessIdentity(os.Getpid(), "malformed"))
}

func TestRuntimeProcessIdentityTreatsUnsupportedVersionAsUnknown(t *testing.T) {
	rec := daemon.RuntimeRecord{
		PID:               os.Getpid(),
		ProcessIdentityV2: "future-v1:12345",
	}
	assert.Equal(t, daemon.ProcessIdentityUnknown,
		daemon.CompareRuntimeProcessIdentity(rec))
}

func TestProcessIdentityTreatsMissingIdentityAsUnknown(t *testing.T) {
	assert.Equal(t, daemon.ProcessIdentityUnknown,
		daemon.CompareProcessIdentity(os.Getpid(), ""))
}

func TestRuntimeProcessScope(t *testing.T) {
	type testCase struct {
		name string
		rec  daemon.RuntimeRecord
		want daemon.ProcessScope
	}
	tests := []testCase{
		{"no versioned identity", daemon.RuntimeRecord{PID: os.Getpid()}, daemon.ProcessScopeLocal},
		{"unsupported identity", daemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: "future-v1:12345"}, daemon.ProcessScopeUnknown},
	}
	if runtime.GOOS == "linux" {
		rec := daemon.NewRuntimeRecord("tool", "v1", daemon.Endpoint{})
		tests = append(tests, testCase{"earlier boot", withLinuxIdentityField(t, rec, 1, "11111111-2222-3333-4444-555555555555"), daemon.ProcessScopeGone})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, daemon.RuntimeProcessScope(tt.rec))
		})
	}
}

func TestRuntimeRecordBlocksStart(t *testing.T) {
	pid := daemon.StartLivePIDHelperForTest(t)
	identity, ok := daemon.ReadProcessIdentity(pid)
	require.True(t, ok)
	rec := daemon.RuntimeRecord{PID: pid, ProcessIdentityV2: identity}
	type testCase struct {
		name string
		rec  daemon.RuntimeRecord
		want bool
	}
	tests := []testCase{
		{"self", daemon.NewRuntimeRecord("tool", "v1", daemon.Endpoint{}), false},
		{"live child", rec, true},
		{"mismatched live child", mismatchedRuntimeIdentity(t, rec), false},
		{"unknown live child", daemon.RuntimeRecord{PID: pid, ProcessIdentityV2: "future-v1:12345"}, true},
		{"unknown dead PID", daemon.RuntimeRecord{PID: deadPID(t), ProcessIdentityV2: "future-v1:12345"}, false},
	}
	if runtime.GOOS == "linux" {
		tests = append(tests, testCase{"earlier boot live child", withLinuxIdentityField(t, rec, 1, "11111111-2222-3333-4444-555555555555"), false})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, daemon.RuntimeRecordBlocksStart(tt.rec))
		})
	}
}

func TestIsEphemeralExecutableDetectsTestBinaries(t *testing.T) {
	assert := assert.New(t)

	assert.True(daemon.IsEphemeralExecutable("/tmp/tool.test"))
	assert.True(daemon.IsEphemeralExecutable(`C:\Temp\tool.test.exe`))
	assert.True(daemon.IsEphemeralExecutable(`C:\Temp\tool.TEST.EXE`))
	assert.False(daemon.IsEphemeralExecutable("/usr/local/bin/tool"))
}

func TestIsEphemeralExecutableDetectsGoBuildPaths(t *testing.T) {
	assert.True(t, daemon.IsEphemeralExecutable("/tmp/go-build123/tool"))
	assert.True(t, daemon.IsEphemeralExecutable(`C:\Users\me\AppData\Local\Temp\go-build123\tool.exe`))
}

// TestStartDetachedHelper is not a real test: TestStartDetachedRunsChild
// re-executes the test binary with -test.run targeting it so the detached
// child has observable work to do.
func TestStartDetachedHelper(t *testing.T) {
	marker := os.Getenv("KIT_DAEMON_TEST_MARKER")
	if marker == "" {
		t.Skip("helper process for TestStartDetachedRunsChild")
	}
	require.NoError(t, os.WriteFile(marker, []byte("ok"), 0o600))
}

func TestStartDetachedRunsChild(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	marker := filepath.Join(t.TempDir(), "marker")

	err = daemon.StartDetached(t.Context(), daemon.StartDetachedOptions{
		Executable: exe,
		Args:       []string{"-test.run", "^TestStartDetachedHelper$"},
		Env:        append(os.Environ(), "KIT_DAEMON_TEST_MARKER="+marker),
	})
	require.NoError(t, err)

	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		require.True(t, time.Now().Before(deadline), "detached child never wrote marker file")
		time.Sleep(25 * time.Millisecond) //nolint:kennlint // polls a marker written by a detached child process
	}
}

// TestStartDetachedExitHelper is not a real test:
// TestStartDetachedReportsChildExit re-executes the test binary with
// -test.run targeting it so the detached child exits with a known status.
func TestStartDetachedExitHelper(t *testing.T) {
	if os.Getenv("KIT_DAEMON_TEST_EXIT_HELPER") == "" {
		t.Skip("helper process for TestStartDetachedReportsChildExit")
	}
	os.Exit(3)
}

func TestStartDetachedReportsChildExit(t *testing.T) {
	exe, err := os.Executable()
	require.NoError(t, err)
	exited := make(chan error, 1)

	err = daemon.StartDetached(t.Context(), daemon.StartDetachedOptions{
		Executable: exe,
		Args:       []string{"-test.run", "^TestStartDetachedExitHelper$"},
		Env:        append(os.Environ(), "KIT_DAEMON_TEST_EXIT_HELPER=1"),
		Exited:     func(err error) { exited <- err },
	})
	require.NoError(t, err)

	select {
	case err := <-exited:
		var exitErr *exec.ExitError
		require.ErrorAs(t, err, &exitErr)
		require.Equal(t, 3, exitErr.ExitCode())
	case <-time.After(10 * time.Second):
		require.Fail(t, "Exited was not called after the child exited")
	}
}

func storedStopRecord(t *testing.T, rec daemon.RuntimeRecord) daemon.RuntimeRecord {
	t.Helper()
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(rec)
	require.NoError(t, err)
	records, err := store.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	return records[0]
}

func TestStopInputValidation(t *testing.T) {
	for _, pid := range []int{0, -1} {
		t.Run(strconv.Itoa(pid), func(t *testing.T) {
			requests, checks := 0, 0
			err := daemon.Stop(t.Context(), daemon.RuntimeRecord{PID: pid}, daemon.StopOptions{
				Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
				Exited:  func(context.Context, daemon.RuntimeRecord) (bool, error) { checks++; return true, nil },
			})
			assert.Zero(t, requests)
			assert.Zero(t, checks)
			require.EqualError(t, err, "daemon stop requires a positive PID: "+strconv.Itoa(pid))
		})
	}
	t.Run("missing request", func(t *testing.T) {
		err := daemon.Stop(t.Context(), daemon.RuntimeRecord{PID: os.Getpid()}, daemon.StopOptions{})
		require.EqualError(t, err, "daemon stop request is required")
	})
}

func TestStopMissingProofExitEvidence(t *testing.T) {
	exitErr := errors.New("exit check failed")
	for _, result := range []string{"exited", "running", "error"} {
		t.Run(result, func(t *testing.T) {
			rec := storedStopRecord(t, daemon.RuntimeRecord{PID: os.Getpid(), Network: daemon.NetworkTCP, Address: "127.0.0.1:1"})
			requests, checks := 0, 0
			err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
				Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
				Exited: func(_ context.Context, got daemon.RuntimeRecord) (bool, error) {
					checks++
					assert.Equal(t, rec, got)
					if result == "error" {
						return false, exitErr
					}
					return result == "exited", nil
				},
			})
			assert.Zero(t, requests)
			assert.Equal(t, 1, checks)
			if result == "exited" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, daemon.ErrStopProofRequired)
				require.NotErrorIs(t, err, daemon.ErrDaemonUnreachable)
			}
			if result == "error" {
				assert.ErrorIs(t, err, exitErr)
			}
		})
	}
}

func TestStopStaleEndpointWithoutExited(t *testing.T) {
	for _, network := range []string{daemon.NetworkUnix, daemon.NetworkTCP} {
		t.Run(network, func(t *testing.T) {
			rec := daemon.RuntimeRecord{PID: os.Getpid(), Service: "test", Network: network}
			opts := daemon.StopOptions{}
			if network == daemon.NetworkUnix {
				rec.Address = staleUnixSocket(t)
			} else {
				var lc net.ListenConfig
				listener, err := lc.Listen(t.Context(), daemon.NetworkTCP, "127.0.0.1:0")
				require.NoError(t, err)
				rec.Address = listener.Addr().String()
				require.NoError(t, listener.Close())
				opts.Proof = newDaemonProof(t, []byte("stop-stale-key"))
			}
			rec = storedStopRecord(t, rec)
			requests := 0
			opts.Request = func(context.Context, daemon.RuntimeRecord) error { requests++; return nil }
			err := daemon.Stop(t.Context(), rec, opts)
			assert.Zero(t, requests)
			if network == daemon.NetworkUnix {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, daemon.ErrDaemonUnreachable)
			}
		})
	}
}

func TestStopEndpointWait(t *testing.T) {
	busy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "busy", http.StatusServiceUnavailable) })
	for _, tc := range []struct {
		name           string
		network        string
		after          http.Handler
		closeListener  bool
		timeout        time.Duration
		callerDeadline bool
		wantErr        error
	}{
		{name: "tcp silence", network: daemon.NetworkTCP, closeListener: true},
		{name: "unix socket removed", network: daemon.NetworkUnix, closeListener: true},
		{name: "503 then silence", network: daemon.NetworkTCP, after: busy, closeListener: true},
		{name: "replacement", network: daemon.NetworkTCP, after: daemon.NewPingHandler(daemon.PingHandlerOptions{PID: os.Getpid() + 1, Service: "replacement"})},
		{name: "missing PID after shutdown request", network: daemon.NetworkTCP, after: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		}), timeout: 150 * time.Millisecond, wantErr: context.DeadlineExceeded},
		{name: "context with live endpoint", network: daemon.NetworkTCP, callerDeadline: true, wantErr: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var ping http.Handler
			var requested atomic.Bool
			var probes atomic.Int32
			var listener net.Listener
			closing := make(chan struct{})
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requested.Load() {
					if probes.Add(1) == 2 && tc.closeListener {
						// Keep the listener open after the first response. Reaching
						// this retry proves that a 503 was not accepted as exit.
						defer func() {
							close(closing)
							assert.NoError(t, listener.Close())
						}()
					}
					if tc.after != nil {
						tc.after.ServeHTTP(w, r)
						return
					}
				}
				ping.ServeHTTP(w, r)
			}))
			if tc.network == daemon.NetworkUnix {
				require.NoError(t, server.Listener.Close())
				listener, err := net.ListenUnix(daemon.NetworkUnix, &net.UnixAddr{Name: unixSocketPath(t), Net: daemon.NetworkUnix})
				require.NoError(t, err)
				server.Listener = listener
			}
			listener = server.Listener
			server.Start()
			defer server.Close()
			rec := storedStopRecord(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "original", Network: tc.network, Address: server.Listener.Addr().String()})
			proof := newDaemonProof(t, []byte("stop-wait-key"))
			initialPing, err := proof.NewPingHandler(rec)
			require.NoError(t, err)
			ping = initialPing
			calls := 0
			opts := daemon.StopOptions{
				Proof:   proof,
				Timeout: tc.timeout, Probe: daemon.ProbeOptions{ExpectedService: "original"},
				Request: func(_ context.Context, got daemon.RuntimeRecord) error {
					assert.Equal(t, rec, got)
					calls++
					requested.Store(true)
					return nil
				},
			}
			if tc.network == daemon.NetworkUnix {
				opts.Proof = nil
			}
			ctx := t.Context()
			if tc.callerDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 150*time.Millisecond) //nolint:kennlint // exercises the caller's deadline while the real endpoint remains live
				defer cancel()
			}
			err = daemon.Stop(ctx, rec, opts)
			require.ErrorIs(t, err, tc.wantErr)
			if tc.closeListener {
				select {
				case <-closing:
				default:
					assert.Fail(t, "Stop returned before listener closure began")
				}
				assert.EqualValues(t, 2, probes.Load())
			}
			assert.Equal(t, 1, calls)
			assert.Positive(t, probes.Load())
		})
	}
}

func TestStopExitProofAndErrors(t *testing.T) {
	failure := errors.New("shutdown failed")
	for _, mode := range []string{"closed", "exited with live endpoint", "running with live endpoint", "exit error", "request error"} {
		t.Run(mode, func(t *testing.T) {
			var requested atomic.Bool
			var probes atomic.Int32
			ping := daemon.NewPingHandler(daemon.PingHandlerOptions{PID: os.Getpid()})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requested.Load() {
					probes.Add(1)
				}
				ping.ServeHTTP(w, r)
			}))
			defer server.Close()
			rec := storedStopRecord(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "test", Address: listenerAddr(t, server)})
			proof := newDaemonProof(t, []byte("stop-exit-key"))
			var err error
			ping, err = proof.NewPingHandler(rec)
			require.NoError(t, err)
			checks := 0
			opts := daemon.StopOptions{
				Proof:   proof,
				Timeout: 150 * time.Millisecond,
				Request: func(context.Context, daemon.RuntimeRecord) error {
					requested.Store(true)
					if mode == "closed" {
						server.Close()
					}
					if mode == "request error" {
						return failure
					}
					return nil
				},
				Exited: func(_ context.Context, got daemon.RuntimeRecord) (bool, error) {
					assert.True(t, requested.Load())
					assert.Equal(t, rec, got)
					checks++
					if mode == "exit error" {
						return false, failure
					}
					return mode != "running with live endpoint" && (mode != "closed" || checks == 2), nil
				},
			}
			if mode == "request error" {
				opts.Exited = nil
			}
			err = daemon.Stop(t.Context(), rec, opts)
			switch mode {
			case "running with live endpoint":
				require.ErrorIs(t, err, context.DeadlineExceeded)
			case "request error", "exit error":
				require.ErrorIs(t, err, failure)
			default:
				require.NoError(t, err)
			}
			assert.Zero(t, probes.Load())
			if mode == "closed" {
				assert.Equal(t, 2, checks)
				assert.FileExists(t, rec.SourcePath)
			}
		})
	}
}

func TestStopPreflightExitProof(t *testing.T) {
	failure := errors.New("exit proof failed")
	for _, endpoint := range []string{"unproofed responder", "gone process"} {
		for _, result := range []string{"exited", "running", "error"} {
			t.Run(endpoint+"/"+result, func(t *testing.T) {
				var probes atomic.Int32
				ping := daemon.NewPingHandler(daemon.PingHandlerOptions{PID: os.Getpid(), Service: "test", Version: "v1"})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					probes.Add(1)
					ping.ServeHTTP(w, r)
				}))
				defer server.Close()
				rec := daemon.RuntimeRecord{PID: os.Getpid(), Service: "test", Version: "v1", Address: listenerAddr(t, server)}
				if endpoint == "gone process" {
					rec.PID = deadPID(t)
				}
				rec = storedStopRecord(t, rec)
				requests, checks := 0, 0
				err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
					Proof:   newDaemonProof(t, []byte("preflight-key")),
					Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
					Exited: func(_ context.Context, got daemon.RuntimeRecord) (bool, error) {
						checks++
						assert.Equal(t, rec, got)
						if endpoint == "unproofed responder" {
							assert.Positive(t, probes.Load(), "preflight must precede exit proof")
						}
						if result == "error" {
							return false, failure
						}
						return result == "exited", nil
					},
				})
				assert.Zero(t, requests)
				if endpoint == "gone process" {
					assert.Zero(t, probes.Load())
				}
				assert.Equal(t, 1, checks)
				switch result {
				case "exited":
					require.NoError(t, err)
				case "running":
					if endpoint == "gone process" {
						require.ErrorIs(t, err, daemon.ErrExitUnconfirmed)
						require.NotErrorIs(t, err, daemon.ErrDaemonUnreachable)
					} else {
						require.ErrorIs(t, err, daemon.ErrDaemonUnreachable)
					}
				case "error":
					require.ErrorIs(t, err, failure)
					if endpoint == "unproofed responder" {
						require.ErrorIs(t, err, daemon.ErrDaemonUnreachable)
					}
					assert.NotEqual(t, failure, err, "exit proof errors must be wrapped")
				}
			})
		}
	}
}

func TestStopGoneProcessProbeContext(t *testing.T) {
	pid := deadPID(t)
	for _, mode := range []string{"canceled", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var probes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				probes.Add(1)
				if mode == "canceled" {
					cancel()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			rec := daemon.RuntimeRecord{PID: pid, Address: listenerAddr(t, server)}
			requests := 0
			err := daemon.Stop(ctx, rec, daemon.StopOptions{
				Timeout: 100 * time.Millisecond,
				Probe:   daemon.ProbeOptions{Timeout: time.Second},
				Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
			})
			assert.EqualValues(t, 1, probes.Load())
			assert.Zero(t, requests)
			want := context.Canceled
			if mode == "timeout" {
				want = context.DeadlineExceeded
			}
			require.ErrorIs(t, err, want)
			assert.NotErrorIs(t, err, daemon.ErrExitUnconfirmed)
		})
	}
}

func TestStopGoneProcessEvidence(t *testing.T) {
	pid := deadPID(t)
	for _, tc := range []struct {
		name    string
		samePID bool
		wantErr bool
	}{
		{name: "recorded PID answering", samePID: true, wantErr: true},
		{name: "gone"},
		{name: "reused PID", samePID: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := daemon.RuntimeRecord{PID: pid, Service: "test"}
			if tc.name == "reused PID" {
				if runtime.GOOS != "linux" {
					t.Skip("requires Linux process identity")
				}
				rec = daemon.NewRuntimeRecord("test", "", daemon.Endpoint{})
				rec = mismatchedRuntimeIdentity(t, rec)
			}
			answerPID := os.Getpid() + 1
			if tc.samePID {
				answerPID = rec.PID
			}
			var probes atomic.Int32
			ping := daemon.NewPingHandler(daemon.PingHandlerOptions{PID: answerPID})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probes.Add(1)
				ping.ServeHTTP(w, r)
			}))
			defer server.Close()
			rec.Address = listenerAddr(t, server)
			rec = storedStopRecord(t, rec)
			requests := 0
			err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
				Proof:   newDaemonProof(t, []byte("gone-process-key")),
				Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
			})
			assert.Zero(t, requests)
			if tc.wantErr {
				require.ErrorIs(t, err, daemon.ErrExitUnconfirmed)
				require.NotErrorIs(t, err, daemon.ErrDaemonUnreachable)
			} else {
				require.NoError(t, err)
			}
			assert.EqualValues(t, 1, probes.Load())
		})
	}
}
