package daemon_test

import (
	"context"
	"errors"
	"net"
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

func TestStopFailedProbeUsesExitEvidence(t *testing.T) {
	exitErr := errors.New("exit check failed")
	for _, result := range []string{"exited", "running", "error"} {
		t.Run(result, func(t *testing.T) {
			endpoint := closedEndpoint(t)
			rec := storedStopRecord(t, daemon.RuntimeRecord{
				PID:     os.Getpid(),
				Network: endpoint.Network,
				Address: endpoint.Address,
			})
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
				require.ErrorIs(t, err, daemon.ErrDaemonUnreachable)
				require.NotErrorIs(t, err, daemon.ErrExitUnconfirmed)
			}
			if result == "error" {
				assert.ErrorIs(t, err, exitErr)
			}
		})
	}
}

func TestStopStaleEndpointWithoutExited(t *testing.T) {
	t.Run("unix socket removed", func(t *testing.T) {
		rec := storedStopRecord(t, daemon.RuntimeRecord{
			PID:     os.Getpid(),
			Service: "test",
			Network: daemon.NetworkUnix,
			Address: staleUnixSocket(t),
		})
		requests := 0
		err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
			Probe:   daemon.ProbeOptions{ExpectedService: "test"},
			Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
		})
		require.NoError(t, err)
		assert.Zero(t, requests)
	})

	t.Run("tcp connection refused", func(t *testing.T) {
		endpoint := closedEndpoint(t)
		rec := storedStopRecord(t, daemon.RuntimeRecord{
			PID:     os.Getpid(),
			Service: "test",
			Network: endpoint.Network,
			Address: endpoint.Address,
		})
		requests := 0
		err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
			Probe:   daemon.ProbeOptions{ExpectedService: "test"},
			Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
		})
		require.ErrorIs(t, err, daemon.ErrDaemonUnreachable)
		assert.Zero(t, requests)
	})
}

func TestStopEndpointWait(t *testing.T) {
	t.Run("tcp daemon closed after request", func(t *testing.T) {
		d := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "original"})
		rec := storedStopRecord(t, daemon.RuntimeRecord{
			PID:       os.Getpid(),
			Service:   "original",
			Network:   d.endpoint.Network,
			Address:   d.endpoint.Address,
			StartedAt: time.Now(),
		})
		calls := 0
		err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
			Probe: daemon.ProbeOptions{ExpectedService: "original"},
			Request: func(context.Context, daemon.RuntimeRecord) error {
				calls++
				d.halt()
				return nil
			},
		})
		require.NoError(t, err)
		assert.Equal(t, 1, calls)
		assert.GreaterOrEqual(t, d.probes.Load(), int32(1))
	})

	t.Run("unix daemon closed after request", func(t *testing.T) {
		d := startTestDaemonUnix(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "original"}, nil)
		rec := storedStopRecord(t, daemon.RuntimeRecord{
			PID:       os.Getpid(),
			Service:   "original",
			Network:   d.endpoint.Network,
			Address:   d.endpoint.Address,
			StartedAt: time.Now(),
		})
		calls := 0
		err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
			Probe: daemon.ProbeOptions{ExpectedService: "original"},
			Request: func(context.Context, daemon.RuntimeRecord) error {
				calls++
				d.halt()
				return nil
			},
		})
		require.NoError(t, err)
		assert.Equal(t, 1, calls)
	})

	t.Run("replacement answers with another PID", func(t *testing.T) {
		var replaced atomic.Bool
		endpoint := newTCPEndpoint(t)
		original := daemon.NewPingHandler(daemon.PingHandlerOptions{
			Record: daemon.RuntimeRecord{PID: os.Getpid(), Service: "original"},
		})
		replacement := daemon.NewPingHandler(daemon.PingHandlerOptions{
			Record: daemon.RuntimeRecord{PID: os.Getpid() + 1, Service: "replacement"},
		})
		listener, err := net.Listen(endpoint.Network, endpoint.Address)
		require.NoError(t, err)
		startTestHandler(t, listener, endpoint, func(ctx context.Context, conn net.Conn) error {
			if replaced.Load() {
				return replacement(ctx, conn)
			}
			return original(ctx, conn)
		})
		rec := storedStopRecord(t, daemon.RuntimeRecord{
			PID:       os.Getpid(),
			Service:   "original",
			Network:   endpoint.Network,
			Address:   endpoint.Address,
			StartedAt: time.Now(),
		})
		err = daemon.Stop(t.Context(), rec, daemon.StopOptions{
			Probe: daemon.ProbeOptions{ExpectedService: "original"},
			Request: func(context.Context, daemon.RuntimeRecord) error {
				replaced.Store(true)
				return nil
			},
		})
		require.NoError(t, err)
	})

	t.Run("non-positive answering PID keeps waiting", func(t *testing.T) {
		var missingPID atomic.Bool
		endpoint := newTCPEndpoint(t)
		withPID := daemon.NewPingHandler(daemon.PingHandlerOptions{
			Record: daemon.RuntimeRecord{PID: os.Getpid(), Service: "original"},
		})
		withoutPID := daemon.NewPingHandler(daemon.PingHandlerOptions{
			Record: daemon.RuntimeRecord{PID: -1, Service: "original"},
		})
		listener, err := net.Listen(endpoint.Network, endpoint.Address)
		require.NoError(t, err)
		startTestHandler(t, listener, endpoint, func(ctx context.Context, conn net.Conn) error {
			if missingPID.Load() {
				return withoutPID(ctx, conn)
			}
			return withPID(ctx, conn)
		})
		rec := storedStopRecord(t, daemon.RuntimeRecord{
			PID:       os.Getpid(),
			Service:   "original",
			Network:   endpoint.Network,
			Address:   endpoint.Address,
			StartedAt: time.Now(),
		})
		err = daemon.Stop(t.Context(), rec, daemon.StopOptions{
			Probe:   daemon.ProbeOptions{ExpectedService: "original"},
			Timeout: 150 * time.Millisecond,
			Request: func(context.Context, daemon.RuntimeRecord) error {
				missingPID.Store(true)
				return nil
			},
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("caller context with live endpoint", func(t *testing.T) {
		d := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "original"})
		rec := storedStopRecord(t, daemon.RuntimeRecord{
			PID:       os.Getpid(),
			Service:   "original",
			Network:   d.endpoint.Network,
			Address:   d.endpoint.Address,
			StartedAt: time.Now(),
		})
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond) //nolint:kennlint // exercises the caller's deadline while the real endpoint remains live
		defer cancel()
		err := daemon.Stop(ctx, rec, daemon.StopOptions{
			Probe:   daemon.ProbeOptions{ExpectedService: "original"},
			Request: func(context.Context, daemon.RuntimeRecord) error { return nil },
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
	})
}

func TestStopExitProofAndErrors(t *testing.T) {
	failure := errors.New("shutdown failed")
	for _, mode := range []string{"closed", "exited with live endpoint", "running with live endpoint", "exit error", "request error"} {
		t.Run(mode, func(t *testing.T) {
			var requested atomic.Bool
			d := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "test"})
			rec := storedStopRecord(t, daemon.RuntimeRecord{
				PID:       os.Getpid(),
				Service:   "test",
				Network:   d.endpoint.Network,
				Address:   d.endpoint.Address,
				StartedAt: time.Now(),
			})
			checks := 0
			opts := daemon.StopOptions{
				Timeout: 150 * time.Millisecond,
				Request: func(context.Context, daemon.RuntimeRecord) error {
					requested.Store(true)
					if mode == "closed" {
						d.halt()
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
			err := daemon.Stop(t.Context(), rec, opts)
			switch mode {
			case "running with live endpoint":
				require.ErrorIs(t, err, context.DeadlineExceeded)
			case "request error", "exit error":
				require.ErrorIs(t, err, failure)
			default:
				require.NoError(t, err)
			}
			assert.GreaterOrEqual(t, d.probes.Load(), int32(1))
			if mode == "closed" {
				assert.Equal(t, 2, checks)
				assert.FileExists(t, rec.SourcePath)
			}
		})
	}
}

func TestStopPreflightExitProof(t *testing.T) {
	failure := errors.New("exit proof failed")
	for _, endpointKind := range []string{"unproofed responder", "gone process"} {
		for _, result := range []string{"exited", "running", "error"} {
			t.Run(endpointKind+"/"+result, func(t *testing.T) {
				d := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "test", Version: "v1"})
				rec := daemon.RuntimeRecord{
					PID:       os.Getpid(),
					Service:   "test",
					Version:   "v1",
					Network:   d.endpoint.Network,
					Address:   d.endpoint.Address,
					StartedAt: time.Now(),
				}
				if endpointKind == "gone process" {
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
						if endpointKind == "unproofed responder" {
							assert.Positive(t, d.probes.Load(), "preflight must precede exit proof")
						}
						if result == "error" {
							return false, failure
						}
						return result == "exited", nil
					},
				})
				assert.Zero(t, requests)
				if endpointKind == "gone process" {
					assert.Zero(t, d.probes.Load())
				}
				assert.Equal(t, 1, checks)
				switch result {
				case "exited":
					require.NoError(t, err)
				case "running":
					if endpointKind == "gone process" {
						require.ErrorIs(t, err, daemon.ErrExitUnconfirmed)
						require.NotErrorIs(t, err, daemon.ErrDaemonUnreachable)
					} else {
						require.ErrorIs(t, err, daemon.ErrDaemonUnreachable)
					}
				case "error":
					require.ErrorIs(t, err, failure)
					if endpointKind == "unproofed responder" {
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
			d := startHangingDaemon(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if mode == "canceled" {
				go func() {
					time.Sleep(20 * time.Millisecond) //nolint:kennlint // cancels the in-flight probe
					cancel()
				}()
			}
			rec := daemon.RuntimeRecord{PID: pid, Address: d.endpoint.Address, Network: d.endpoint.Network}
			requests := 0
			err := daemon.Stop(ctx, rec, daemon.StopOptions{
				Timeout: 100 * time.Millisecond,
				Probe:   daemon.ProbeOptions{Timeout: time.Second},
				Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
			})
			assert.EqualValues(t, 1, d.probes.Load())
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
			endpoint := closedEndpoint(t)
			var d *testDaemon
			if tc.name != "gone" {
				d = startTestDaemon(t, daemon.RuntimeRecord{PID: answerPID, Service: "test"})
				endpoint = d.endpoint
			}
			rec.Network = endpoint.Network
			rec.Address = endpoint.Address
			rec = storedStopRecord(t, rec)
			requests := 0
			err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
				Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
			})
			assert.Zero(t, requests)
			if tc.wantErr {
				require.ErrorIs(t, err, daemon.ErrExitUnconfirmed)
				require.NotErrorIs(t, err, daemon.ErrDaemonUnreachable)
			} else {
				require.NoError(t, err)
			}
			if d != nil {
				assert.EqualValues(t, 1, d.probes.Load())
			}
		})
	}
}
