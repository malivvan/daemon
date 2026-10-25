package daemon_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/malivvan/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewPingHandlerEmitsRequiredPingInfo(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: "roborev",
		Version: "dev",
		PID:     123,
	}))
	defer server.Close()

	resp, err := doRequest(t, server.Client(), http.MethodGet, server.URL, "", nil)
	require.NoError(err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(http.StatusOK, resp.StatusCode)
	assert.Equal("application/json", resp.Header.Get("Content-Type"))
	var info daemon.PingInfo
	require.NoError(json.NewDecoder(resp.Body).Decode(&info))
	assert.True(info.OK)
	assert.Equal("roborev", info.Service)
	assert.Equal("dev", info.Version)
	assert.Equal(123, info.PID)
}

func TestNewPingHandlerRejectsNonGET(t *testing.T) {
	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{}))
	defer server.Close()

	resp, err := doRequest(t, server.Client(), http.MethodPost, server.URL, "application/json", nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	assert.Equal(t, http.MethodGet, resp.Header.Get("Allow"))
}

func TestProbeHTTPRequiresOKTrue(t *testing.T) {
	assert := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(daemon.DefaultPingPath, r.URL.Path)
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"roborev","version":"dev","pid":123}`)
	}))
	defer server.Close()

	info, err := daemon.ProbeHTTP(t.Context(), server.Client(), server.URL, daemon.ProbeOptions{
		ExpectedService: "roborev",
	})
	require.NoError(t, err)
	assert.True(info.OK)
	assert.Equal("roborev", info.Service)
	assert.Equal("dev", info.Version)
	assert.Equal(123, info.PID)
}

func TestProbeHTTPRejectsOKOmitted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"service":"kata"}`)
	}))
	defer server.Close()

	_, err := daemon.ProbeHTTP(t.Context(), server.Client(), server.URL, daemon.ProbeOptions{
		ExpectedService: "kata",
	})
	require.Error(t, err)
}

func TestProbeHTTPRejectsOKFalse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":false,"service":"kata"}`)
	}))
	defer server.Close()

	_, err := daemon.ProbeHTTP(t.Context(), server.Client(), server.URL, daemon.ProbeOptions{
		ExpectedService: "kata",
	})
	require.Error(t, err)
}

func TestProbeHTTPAppliesTimeoutOption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) //nolint:kennlint // server-side delay drives the real HTTP client timeout
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata"}`)
	}))
	defer server.Close()

	_, err := daemon.ProbeHTTP(t.Context(), server.Client(), server.URL, daemon.ProbeOptions{
		Timeout: time.Millisecond,
	})
	require.Error(t, err)
}

func TestDiscoverFindsResponsiveRuntime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","version":"v1","pid":%d}`, os.Getpid())
	}))
	defer server.Close()

	addr := listenerAddr(t, server)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   addr,
		Service:   "kata",
		Version:   "v1",
		StartedAt: time.Now(),
	})
	require.NoError(err)

	rec, info, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(addr, rec.Address)
	assert.Equal("kata", info.Service)
}

func TestDiscoverRejectsPIDMismatchWhenRequiringLivePID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","pid":%d}`, os.Getpid()+1)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, server),
		Service:   "kata",
		StartedAt: time.Now(),
	})
	require.NoError(t, err)

	_, _, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestDiscoverSkipsMismatchedProcessIdentityWithoutProbing(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	livePID := daemon.StartLivePIDHelperForTest(t)
	recordedIdentity, ok := daemon.ReadProcessIdentity(os.Getpid())
	require.True(ok)
	require.Equal(daemon.ProcessIdentityMismatch,
		daemon.CompareProcessIdentity(livePID, recordedIdentity))

	var probes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		http.Error(w, "must not be probed", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:             livePID,
		ProcessIdentity: recordedIdentity,
		Network:         daemon.NetworkTCP,
		Address:         listenerAddr(t, server),
		Service:         "kata",
		StartedAt:       time.Now(),
	})
	require.NoError(err)

	_, _, found, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.NoError(err)
	assert.False(found)
	assert.Zero(probes.Load())
}

func TestDiscoverWithoutPIDCheckKeepsFailedProbeAsAbsence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.NewRuntimeRecord("kata", "v1", daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: listenerAddr(t, server),
	}))
	require.NoError(t, err)

	_, _, found, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe: daemon.ProbeOptions{ExpectedService: "kata"},
	})
	require.NoError(t, err)
	assert.False(t, found)
}

func TestDiscoverReturnsUnreachableErrorForLiveRuntime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	rec := daemon.NewRuntimeRecord("kata", "v1", daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: listenerAddr(t, server),
	})
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(rec)
	require.NoError(err)

	_, _, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.Error(err)
	assert.False(ok)
	require.ErrorIs(err, daemon.ErrDaemonUnreachable)
	unreachable, ok := errors.AsType[*daemon.UnreachableError](err)
	if !ok || unreachable == nil {
		require.FailNow("expected UnreachableError")
		return
	}
	assert.Equal(rec.PID, unreachable.Record.PID)
	assert.Equal(rec.Endpoint(), unreachable.Endpoint)
	require.Error(unreachable.Err)
	require.ErrorIs(err, unreachable.Err)
}

func TestDiscoverScansPastUnreachableLiveRuntime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	unreachableServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer unreachableServer.Close()
	reachableServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"kata","pid":%d}`, os.Getpid())
	}))
	defer reachableServer.Close()

	livePID := daemon.StartLivePIDHelperForTest(t)
	identity, ok := daemon.ReadProcessIdentity(livePID)
	require.True(ok)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:             livePID,
		ProcessIdentity: identity,
		Network:         daemon.NetworkTCP,
		Address:         listenerAddr(t, unreachableServer),
		Service:         "kata",
		StartedAt:       time.Now().Add(-time.Minute),
	})
	require.NoError(err)
	reachable := daemon.NewRuntimeRecord("kata", "v1", daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: listenerAddr(t, reachableServer),
	})
	_, err = store.Write(reachable)
	require.NoError(err)

	found, info, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(reachable.PID, found.PID)
	assert.Equal("kata", info.Service)
}

func TestManagerFindDoesNotDiscloseCredentialBeforeProof(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	livePID := daemon.StartLivePIDHelperForTest(t)
	assert.NotEqual(os.Getpid(), livePID)
	token := []byte("persistent-daemon-token")
	proof := newDaemonProof(t, token)
	var credentialDisclosed atomic.Bool
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			credentialDisclosed.Store(true)
		}
		if r.Header.Get("Authorization") != "" ||
			requestContainsSecret(r, body, token) {
			credentialDisclosed.Store(true)
		}
		_, _ = fmt.Fprintf(w, `{"ok":true,"service":"tool","version":"v1","pid":%d}`, livePID)
	}))
	defer attacker.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       livePID,
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, attacker),
		Service:   "tool",
		Version:   "v1",
		StartedAt: time.Now().Add(-time.Hour),
	})
	require.NoError(err)

	manager := daemon.Manager{
		Store: store,
		Discover: daemon.DiscoverOptions{
			Probe:           daemon.ProbeOptions{ExpectedService: "tool"},
			RequirePIDAlive: true,
			Proof:           proof,
		},
	}

	_, _, ok, err := manager.Find(t.Context())
	require.Error(err)
	assert.False(ok)
	require.ErrorIs(err, daemon.ErrDaemonUnreachable)
	assert.False(credentialDisclosed.Load(), "credential material reached an unproved endpoint")
}

func TestDiscoverAcceptsValidRuntimeProof(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	key := []byte("valid-proof-key")
	serverProof := newDaemonProof(t, key)
	server, rec := startProofServer(t, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Service:   "tool",
		Version:   "v1",
		StartedAt: time.Now(),
	}, serverProof)
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(rec)
	require.NoError(err)

	found, info, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "tool"},
		RequirePIDAlive: true,
		Proof:           newDaemonProof(t, key),
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(rec.Address, found.Address)
	assert.Equal(rec.PID, info.PID)
	assert.Equal(rec.Service, info.Service)
}

func TestProofPingHandlerPreservesStandardReadiness(t *testing.T) {
	assert := assert.New(t)

	server, rec := startProofServer(t, daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Service: "tool",
		Version: "v1",
	}, newDaemonProof(t, []byte("readiness-proof-key")))
	defer server.Close()

	info, err := daemon.Probe(t.Context(), rec.Endpoint(), daemon.ProbeOptions{
		ExpectedService: "tool",
	})
	require.NoError(t, err)
	assert.True(info.OK)
	assert.Equal(rec.PID, info.PID)
	assert.Equal(rec.Version, info.Version)
}

func TestProofProbeRejectsWrongKey(t *testing.T) {
	serverProof := newDaemonProof(t, []byte("server-proof-key"))
	server, rec := startProofServer(t, daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Service: "tool",
		Version: "v1",
	}, serverProof)
	defer server.Close()

	clientProof := newDaemonProof(t, []byte("different-proof-key"))
	_, err := clientProof.Probe(t.Context(), rec, daemon.ProbeOptions{
		ExpectedService: "tool",
	})
	require.Error(t, err)
}

func TestProofFormattingDoesNotDiscloseKey(t *testing.T) {
	assert := assert.New(t)
	key := []byte("format-sensitive-key")
	proof := newDaemonProof(t, key)
	manager := daemon.Manager{Discover: daemon.DiscoverOptions{Proof: proof}}
	byteList := fmt.Sprintf("%d", key)

	values := []struct {
		name  string
		value any
	}{
		{name: "pointer", value: proof},
		{name: "value", value: *proof},
		{name: "manager", value: manager},
	}
	for _, value := range values {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			rendered := fmt.Sprintf(format, value.value)
			disclosed := strings.Contains(rendered, string(key)) || strings.Contains(rendered, byteList)
			assert.False(disclosed, "%s formatting disclosed proof key material", value.name)
		}
	}
}

func TestDiscoverPropagatesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, _, err := daemon.Discover(ctx, daemon.RuntimeStore{Dir: t.TempDir()}, daemon.DiscoverOptions{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestDiscoverSkipsPerProbeTimeouts(t *testing.T) {
	require := require.New(t)

	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer slowServer.Close()
	fastServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata","version":"fast"}`)
	}))
	defer fastServer.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, slowServer),
		Service:   "kata",
		StartedAt: time.Now().Add(-time.Minute),
	})
	require.NoError(err)
	_, err = store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid() + 1,
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, fastServer),
		Service:   "kata",
		StartedAt: time.Now(),
	})
	require.NoError(err)

	_, info, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe: daemon.ProbeOptions{ExpectedService: "kata", Timeout: 100 * time.Millisecond},
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(t, "fast", info.Version)
}

func TestProbeDialsUnixEndpoint(t *testing.T) {
	require := require.New(t)

	socketDir, err := os.MkdirTemp("", "kitd") //nolint:usetesting // unix socket paths must stay short, so the test needs a fixed OS temp root
	require.NoError(err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "daemon.sock")
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", socketPath)
	require.NoError(err)
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})

	server := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata"}`)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Shutdown(context.WithoutCancel(t.Context())) })

	_, err = daemon.Probe(t.Context(), daemon.Endpoint{
		Network: daemon.NetworkUnix,
		Address: socketPath,
	}, daemon.ProbeOptions{ExpectedService: "kata"})
	require.NoError(err)
}

func TestManagerEnsureStartsAndPollsForCompatibleDaemon(t *testing.T) {
	assert := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"v2"}`)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	started := false
	manager := daemon.Manager{
		Store: store,
		Discover: daemon.DiscoverOptions{
			Probe: daemon.ProbeOptions{ExpectedService: "tool"},
		},
		Compatible: func(_ daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return info.Version == "v2"
		},
		Start: func(context.Context) error {
			started = true
			_, err := store.Write(daemon.RuntimeRecord{
				PID:       os.Getpid(),
				Network:   daemon.NetworkTCP,
				Address:   listenerAddr(t, server),
				Service:   "tool",
				Version:   "v2",
				StartedAt: time.Now(),
			})
			return err
		},
	}

	rec, info, err := manager.Ensure(t.Context(), time.Second)
	require.NoError(t, err)
	assert.True(started)
	assert.Equal("tool", rec.Service)
	assert.Equal("v2", info.Version)
}

func TestManagerFindSkipsIncompatibleDaemon(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"old"}`)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, server),
		Service:   "tool",
		Version:   "old",
		StartedAt: time.Now(),
	})
	require.NoError(t, err)
	manager := daemon.Manager{
		Store:    store,
		Discover: daemon.DiscoverOptions{Probe: daemon.ProbeOptions{ExpectedService: "tool"}},
		Compatible: func(_ daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return info.Version == "new"
		},
	}

	_, _, ok, err := manager.Find(t.Context())
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestManagerEnsureDoesNotStartWhenDiscoveryIsUnreachable(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.NewRuntimeRecord("tool", "v1", daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: listenerAddr(t, server),
	}))
	require.NoError(err)

	started := false
	manager := daemon.Manager{
		Store: store,
		Discover: daemon.DiscoverOptions{
			Probe:           daemon.ProbeOptions{ExpectedService: "tool"},
			RequirePIDAlive: true,
		},
		Start: func(context.Context) error {
			started = true
			return nil
		},
	}

	_, _, err = manager.Ensure(t.Context(), time.Second)
	require.Error(err)
	require.ErrorIs(err, daemon.ErrDaemonUnreachable)
	assert.False(started)
}

func TestManagerFindScansPastIncompatibleDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"old"}`)
	}))
	defer oldServer.Close()
	newServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"new"}`)
	}))
	defer newServer.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, oldServer),
		Service:   "tool",
		Version:   "old",
		StartedAt: time.Now().Add(-time.Minute),
	})
	require.NoError(err)
	_, err = store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid() + 1,
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, newServer),
		Service:   "tool",
		Version:   "new",
		StartedAt: time.Now(),
	})
	require.NoError(err)

	manager := daemon.Manager{
		Store:    store,
		Discover: daemon.DiscoverOptions{Probe: daemon.ProbeOptions{ExpectedService: "tool"}},
		Compatible: func(_ daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return info.Version == "new"
		},
	}

	rec, info, ok, err := manager.Find(t.Context())
	require.NoError(err)
	require.True(ok)
	assert.Equal(listenerAddr(t, newServer), rec.Address)
	assert.Equal("new", info.Version)
}

func TestManagerFindUsesCustomDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Probe") != "present" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"v1"}`)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, server),
		Service:   "tool",
		Version:   "v1",
		StartedAt: time.Now(),
	})
	require.NoError(err)

	manager := daemon.Manager{
		Store:    store,
		FindFunc: discoverWithHeader(store, "X-Test-Probe", "present"),
	}
	rec, info, ok, err := manager.Find(t.Context())
	require.NoError(err)
	require.True(ok)
	assert.Equal(listenerAddr(t, server), rec.Address)
	assert.Equal("tool", info.Service)
}

func TestManagerEnsureSerializesConcurrentStarts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"v1"}`)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	var starts atomic.Int32
	manager := daemon.Manager{
		Store:    store,
		Discover: daemon.DiscoverOptions{Probe: daemon.ProbeOptions{ExpectedService: "tool"}},
		Start: func(context.Context) error {
			starts.Add(1)
			time.Sleep(20 * time.Millisecond) //nolint:kennlint // simulates a slow start hook the manager must wait on
			_, err := store.Write(daemon.RuntimeRecord{
				PID:       os.Getpid(),
				Network:   daemon.NetworkTCP,
				Address:   listenerAddr(t, server),
				Service:   "tool",
				Version:   "v1",
				StartedAt: time.Now(),
			})
			return err
		},
	}

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, _, err := manager.Ensure(t.Context(), time.Second)
			errs <- err
		}()
	}
	for range 2 {
		err := <-errs
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), starts.Load())
}

func TestRuntimeStoreOwnerLockExcludesASecondOwner(t *testing.T) {
	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "tool"}
	release, err := store.AcquireOwnerLock(t.Context())
	require.NoError(t, err)
	defer release()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond) //nolint:kennlint // the deadline is the expected result; the first owner's file lock blocks the second acquire until it fires
	defer cancel()
	_, err = store.AcquireOwnerLock(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestRuntimeStoreStartLockIsPubliclyAcquirable(t *testing.T) {
	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "tool"}
	release, err := store.AcquireStartLock(t.Context())
	require.NoError(t, err)
	release()
}

func TestRuntimeStoreOwnerLockDoesNotBlockAnotherPrefixStartLock(t *testing.T) {
	dir := t.TempDir()
	ownerStore := daemon.RuntimeStore{Dir: dir, Prefix: "tool"}
	releaseOwner, err := ownerStore.AcquireOwnerLock(t.Context())
	require.NoError(t, err)
	defer releaseOwner()

	startStore := daemon.RuntimeStore{Dir: dir, Prefix: "tool.owner"}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	releaseStart, err := startStore.AcquireStartLock(ctx)
	require.NoError(t, err)
	releaseStart()
}

func TestManagerEnsureSerializesConcurrentStartsWithCustomDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	var customProbes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test-Probe") != "present" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		customProbes.Add(1)
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"v1"}`)
	}))
	defer server.Close()

	discoveryStore := daemon.RuntimeStore{Dir: t.TempDir()}
	lockStore := daemon.RuntimeStore{Dir: t.TempDir()}
	findCalls := make(chan struct{}, 8)
	find := discoverWithHeader(discoveryStore, "X-Test-Probe", "present")
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseStart:
		default:
			close(releaseStart)
		}
	})

	var starts atomic.Int32
	manager := daemon.Manager{
		Store: lockStore,
		FindFunc: func(ctx context.Context) (daemon.RuntimeRecord, daemon.PingInfo, bool, error) {
			rec, info, ok, err := find(ctx)
			findCalls <- struct{}{}
			return rec, info, ok, err
		},
		Start: func(ctx context.Context) error {
			if starts.Add(1) == 1 {
				close(startEntered)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-releaseStart:
			}
			_, err := discoveryStore.Write(daemon.RuntimeRecord{
				PID:       os.Getpid(),
				Network:   daemon.NetworkTCP,
				Address:   listenerAddr(t, server),
				Service:   "tool",
				Version:   "v1",
				StartedAt: time.Now(),
			})
			return err
		},
	}

	type result struct {
		rec  daemon.RuntimeRecord
		info daemon.PingInfo
		err  error
	}
	results := make(chan result, 2)
	ensure := func() {
		rec, info, err := manager.Ensure(t.Context(), 2*time.Second)
		results <- result{rec: rec, info: info, err: err}
	}

	go ensure()
	requireSignal(t, startEntered)
	requireSignal(t, findCalls)
	requireSignal(t, findCalls)
	go ensure()
	requireSignal(t, findCalls)
	close(releaseStart)

	for range 2 {
		result := <-results
		require.NoError(result.err)
		assert.Equal(listenerAddr(t, server), result.rec.Address)
		assert.Equal("tool", result.info.Service)
	}
	assert.Equal(int32(1), starts.Load())
	assert.Equal(int32(2), customProbes.Load())
}

func TestManagerEnsureAppliesTimeoutToStartLock(t *testing.T) {
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	lockPath, err := store.LockPath()
	require.NoError(err)
	require.NoError(os.MkdirAll(store.Dir, 0o700))
	release, err := daemon.HoldLockForTest(lockPath)
	require.NoError(err)
	defer release()

	manager := daemon.Manager{
		Store: store,
		Start: func(context.Context) error {
			require.FailNow("start should not run while lock is held")
			return nil
		},
	}

	startedAt := time.Now()
	_, _, err = manager.Ensure(t.Context(), 50*time.Millisecond)
	require.Error(err)
	assert.Less(t, time.Since(startedAt), 500*time.Millisecond)
}

func discoverWithHeader(store daemon.RuntimeStore, name, value string) func(context.Context) (daemon.RuntimeRecord, daemon.PingInfo, bool, error) {
	return func(ctx context.Context) (daemon.RuntimeRecord, daemon.PingInfo, bool, error) {
		records, err := store.List()
		if err != nil {
			return daemon.RuntimeRecord{}, daemon.PingInfo{}, false, err
		}
		for _, rec := range records {
			ep := rec.Endpoint()
			client := ep.HTTPClient(daemon.HTTPClientOptions{DisableKeepAlives: true})
			client.Transport = headerTransport{
				base:  client.Transport,
				name:  name,
				value: value,
			}
			info, err := daemon.ProbeHTTP(ctx, client, ep.BaseURL(), daemon.ProbeOptions{ExpectedService: "tool"})
			if err == nil {
				return rec, info, true, nil
			}
		}
		return daemon.RuntimeRecord{}, daemon.PingInfo{}, false, nil
	}
}

type headerTransport struct {
	base  http.RoundTripper
	name  string
	value string
}

func (t headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set(t.name, t.value)
	return t.base.RoundTrip(req)
}

func requireSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		require.FailNow(t, "timed out waiting for test signal")
	}
}

func listenerAddr(t *testing.T, server *httptest.Server) string {
	t.Helper()
	addr := server.Listener.Addr().String()
	require.NoError(t, func() error {
		_, _, err := net.SplitHostPort(addr)
		return err
	}(), "server address %q", addr)
	return addr
}

func requestContainsSecret(r *http.Request, body, secret []byte) bool {
	if bytes.Contains([]byte(r.URL.String()), secret) || bytes.Contains(body, secret) {
		return true
	}
	for _, values := range r.Header {
		for _, value := range values {
			if strings.Contains(value, string(secret)) {
				return true
			}
		}
	}
	return false
}

func newDaemonProof(t *testing.T, key []byte) *daemon.Proof {
	t.Helper()
	proof, err := daemon.NewProof(key)
	require.NoError(t, err)
	return proof
}

func startProofServer(t *testing.T, rec daemon.RuntimeRecord, proof *daemon.Proof) (*httptest.Server, daemon.RuntimeRecord) {
	t.Helper()
	require := require.New(t)

	server := httptest.NewUnstartedServer(nil)
	rec.Network = daemon.NetworkTCP
	rec.Address = server.Listener.Addr().String()
	handler, err := proof.NewPingHandler(rec)
	require.NoError(err)
	server.Config.Handler = handler
	server.Start()
	return server, rec
}

// doRequest issues a request bound to the test context so the linters see
// cancellation plumbed through instead of the context-free Get/Post helpers.
func doRequest(t *testing.T, client *http.Client, method, url, contentType string, body io.Reader) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	require.NoError(t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return client.Do(req)
}
