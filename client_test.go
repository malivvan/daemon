package daemon_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/malivvan/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDaemon serves the daemon handshake on a local listener.
type testDaemon struct {
	endpoint daemon.Endpoint
	listener net.Listener
	probes   atomic.Int32

	done chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

func newTestDaemon(listener net.Listener, endpoint daemon.Endpoint) *testDaemon {
	return &testDaemon{
		endpoint: endpoint,
		listener: listener,
		done:     make(chan struct{}),
	}
}

func (d *testDaemon) serve(handler daemon.PingHandler) {
	for {
		conn, err := d.listener.Accept()
		if err != nil {
			return
		}
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer func() { _ = conn.Close() }()
			d.probes.Add(1)
			_ = handler(context.Background(), conn)
		}()
	}
}

func (d *testDaemon) halt() {
	d.once.Do(func() {
		close(d.done)
		_ = d.listener.Close()
	})
	d.wg.Wait()
}

func startTestDaemon(t *testing.T, rec daemon.RuntimeRecord) *testDaemon {
	t.Helper()
	return startTestDaemonProof(t, rec, nil)
}

func startTestDaemonProof(t *testing.T, rec daemon.RuntimeRecord, proof *daemon.Proof) *testDaemon {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	endpoint := daemon.Endpoint{Network: daemon.NetworkTCP, Address: listener.Addr().String()}
	return serveTestDaemon(t, listener, endpoint, rec, proof)
}

func startTestDaemonUnix(t *testing.T, rec daemon.RuntimeRecord, proof *daemon.Proof) *testDaemon {
	t.Helper()
	endpoint := daemon.Endpoint{Network: daemon.NetworkUnix, Address: unixSocketPath(t)}
	listener, err := net.Listen("unix", endpoint.Address)
	require.NoError(t, err)
	return serveTestDaemon(t, listener, endpoint, rec, proof)
}

func serveTestDaemon(t *testing.T, listener net.Listener, endpoint daemon.Endpoint, rec daemon.RuntimeRecord, proof *daemon.Proof) *testDaemon {
	t.Helper()
	rec.Network = endpoint.Network
	rec.Address = endpoint.Address
	var handler daemon.PingHandler
	if proof != nil {
		var err error
		handler, err = proof.NewPingHandler(rec)
		require.NoError(t, err)
	} else {
		handler = daemon.NewPingHandler(daemon.PingHandlerOptions{Record: rec})
	}
	return startTestHandler(t, listener, endpoint, handler)
}

func startTestHandler(t *testing.T, listener net.Listener, endpoint daemon.Endpoint, handler daemon.PingHandler) *testDaemon {
	t.Helper()
	d := newTestDaemon(listener, endpoint)
	t.Cleanup(d.halt)
	go d.serve(handler)
	return d
}

// startHangingDaemon accepts connections and never answers the handshake.
func startHangingDaemon(t *testing.T) *testDaemon {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	endpoint := daemon.Endpoint{Network: daemon.NetworkTCP, Address: listener.Addr().String()}
	d := newTestDaemon(listener, endpoint)
	t.Cleanup(d.halt)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			d.wg.Add(1)
			go func() {
				defer d.wg.Done()
				defer func() { _ = conn.Close() }()
				d.probes.Add(1)
				<-d.done
			}()
		}
	}()
	return d
}

// newTCPEndpoint reserves and releases a local TCP address.
func newTCPEndpoint(t *testing.T) daemon.Endpoint {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	endpoint := daemon.Endpoint{Network: daemon.NetworkTCP, Address: listener.Addr().String()}
	require.NoError(t, listener.Close())
	return endpoint
}

// closedEndpoint returns an endpoint that refuses connections.
func closedEndpoint(t *testing.T) daemon.Endpoint {
	t.Helper()
	return newTCPEndpoint(t)
}

func storedRecord(t *testing.T, store daemon.RuntimeStore, rec daemon.RuntimeRecord) daemon.RuntimeRecord {
	t.Helper()
	_, err := store.Write(rec)
	require.NoError(t, err)
	return rec
}

func TestPingHandlerAnnouncesRecord(t *testing.T) {
	assert := assert.New(t)
	d := startTestDaemon(t, daemon.RuntimeRecord{Service: "roborev", Version: "dev", PID: 123})

	info, err := daemon.Probe(t.Context(), d.endpoint, daemon.ProbeOptions{ExpectedService: "roborev"})
	require.NoError(t, err)
	assert.True(info.OK)
	assert.Equal("roborev", info.Service)
	assert.Equal("dev", info.Version)
	assert.Equal(123, info.PID)
}

func TestProbeRejectsUnexpectedService(t *testing.T) {
	d := startTestDaemon(t, daemon.RuntimeRecord{Service: "kata", PID: 123})

	_, err := daemon.Probe(t.Context(), d.endpoint, daemon.ProbeOptions{ExpectedService: "other"})
	require.Error(t, err)
}

func TestProbeAppliesTimeoutOption(t *testing.T) {
	d := startHangingDaemon(t)

	_, err := daemon.Probe(t.Context(), d.endpoint, daemon.ProbeOptions{Timeout: 50 * time.Millisecond})
	require.Error(t, err)
}

func TestDialerOptionsBoundHandshake(t *testing.T) {
	d := startHangingDaemon(t)

	_, err := d.endpoint.Dialer(daemon.DialerOptions{Timeout: 50 * time.Millisecond}).DialContext(t.Context())
	require.Error(t, err)
}

func TestDiscoverFindsResponsiveRuntime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "kata", Version: "v1"})
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	rec := storedRecord(t, store, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   d.endpoint.Network,
		Address:   d.endpoint.Address,
		Service:   "kata",
		Version:   "v1",
		StartedAt: time.Now(),
	})

	found, info, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(rec.Address, found.Address)
	assert.Equal("kata", info.Service)
}

func TestDiscoverRejectsPIDMismatchWhenRequiringLivePID(t *testing.T) {
	d := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid() + 1, Service: "kata"})
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	storedRecord(t, store, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   d.endpoint.Network,
		Address:   d.endpoint.Address,
		Service:   "kata",
		StartedAt: time.Now(),
	})

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

	d := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "kata"})
	livePID := daemon.StartLivePIDHelperForTest(t)
	recordedIdentity, ok := daemon.ReadProcessIdentity(os.Getpid())
	require.True(ok)
	require.Equal(daemon.ProcessIdentityMismatch,
		daemon.CompareProcessIdentity(livePID, recordedIdentity))

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	storedRecord(t, store, daemon.RuntimeRecord{
		PID:             livePID,
		ProcessIdentity: recordedIdentity,
		Network:         d.endpoint.Network,
		Address:         d.endpoint.Address,
		Service:         "kata",
		StartedAt:       time.Now(),
	})

	_, _, found, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.NoError(err)
	assert.False(found)
	assert.Zero(d.probes.Load())
}

func TestDiscoverWithoutPIDCheckKeepsFailedProbeAsAbsence(t *testing.T) {
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	storedRecord(t, store, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   closedEndpoint(t).Address,
		Service:   "kata",
		StartedAt: time.Now(),
	})

	_, _, found, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe: daemon.ProbeOptions{ExpectedService: "kata"},
	})
	require.NoError(t, err)
	assert.False(t, found)
}

func TestDiscoverReturnsUnreachableErrorForLiveRuntime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	ep := closedEndpoint(t)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	rec := storedRecord(t, store, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   ep.Network,
		Address:   ep.Address,
		Service:   "kata",
		StartedAt: time.Now(),
	})

	_, _, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.Error(err)
	assert.False(ok)
	require.ErrorIs(err, daemon.ErrDaemonUnreachable)
	var unreachable *daemon.UnreachableError
	require.True(errors.As(err, &unreachable))
	assert.Equal(rec.PID, unreachable.Record.PID)
	assert.Equal(rec.Endpoint(), unreachable.Endpoint)
	require.Error(unreachable.Err)
	require.ErrorIs(err, unreachable.Err)
}

func TestDiscoverScansPastUnreachableLiveRuntime(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	livePID := daemon.StartLivePIDHelperForTest(t)
	identity, ok := daemon.ReadProcessIdentity(livePID)
	require.True(ok)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	storedRecord(t, store, daemon.RuntimeRecord{
		PID:             livePID,
		ProcessIdentity: identity,
		Network:         daemon.NetworkTCP,
		Address:         closedEndpoint(t).Address,
		Service:         "kata",
		StartedAt:       time.Now().Add(-time.Minute),
	})

	d := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "kata", Version: "v1"})
	reachable := storedRecord(t, store, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   d.endpoint.Network,
		Address:   d.endpoint.Address,
		Service:   "kata",
		Version:   "v1",
		StartedAt: time.Now(),
	})

	found, info, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(reachable.PID, found.PID)
	assert.Equal("kata", info.Service)
}

func TestManagerFindFailsProofForUnprovenEndpoint(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "tool", Version: "v1"})
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	storedRecord(t, store, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   d.endpoint.Network,
		Address:   d.endpoint.Address,
		Service:   "tool",
		Version:   "v1",
		StartedAt: time.Now(),
	})

	manager := daemon.Manager{
		Store: store,
		Discover: daemon.DiscoverOptions{
			Probe:           daemon.ProbeOptions{ExpectedService: "tool"},
			RequirePIDAlive: true,
			Proof:           newDaemonProof(t, []byte("persistent-daemon-token")),
		},
	}

	_, _, ok, err := manager.Find(t.Context())
	require.Error(err)
	assert.False(ok)
	require.ErrorIs(err, daemon.ErrDaemonUnreachable)
}

func TestDiscoverAcceptsValidRuntimeProof(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	key := []byte("valid-proof-key")
	proof := newDaemonProof(t, key)
	d := startTestDaemonProof(t, daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Service: "tool",
		Version: "v1",
	}, proof)

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	rec := storedRecord(t, store, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   d.endpoint.Network,
		Address:   d.endpoint.Address,
		Service:   "tool",
		Version:   "v1",
		StartedAt: time.Now(),
	})

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

	d := startTestDaemonProof(t, daemon.RuntimeRecord{
		PID:     os.Getpid(),
		Service: "tool",
		Version: "v1",
	}, newDaemonProof(t, []byte("readiness-proof-key")))

	info, err := daemon.Probe(t.Context(), d.endpoint, daemon.ProbeOptions{ExpectedService: "tool"})
	require.NoError(t, err)
	assert.True(info.OK)
	assert.Equal("tool", info.Service)
	assert.Equal("v1", info.Version)
	assert.Equal(os.Getpid(), info.PID)
}

func TestProofProbeRejectsWrongKey(t *testing.T) {
	rec := daemon.RuntimeRecord{PID: os.Getpid(), Service: "tool", Version: "v1"}
	d := startTestDaemonProof(t, rec, newDaemonProof(t, []byte("server-proof-key")))
	rec.Network = d.endpoint.Network
	rec.Address = d.endpoint.Address

	clientProof := newDaemonProof(t, []byte("different-proof-key"))
	_, err := clientProof.Probe(t.Context(), rec, daemon.ProbeOptions{ExpectedService: "tool"})
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

	slow := startHangingDaemon(t)
	fast := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid() + 1, Service: "kata", Version: "fast"})

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	storedRecord(t, store, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   slow.endpoint.Network,
		Address:   slow.endpoint.Address,
		Service:   "kata",
		StartedAt: time.Now().Add(-time.Minute),
	})
	storedRecord(t, store, daemon.RuntimeRecord{
		PID:       os.Getpid() + 1,
		Network:   fast.endpoint.Network,
		Address:   fast.endpoint.Address,
		Service:   "kata",
		StartedAt: time.Now(),
	})

	_, info, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
		Probe: daemon.ProbeOptions{ExpectedService: "kata", Timeout: 100 * time.Millisecond},
	})
	require.NoError(err)
	require.True(ok)
	assert.Equal(t, "fast", info.Version)
}

func TestProbeDialsUnixEndpoint(t *testing.T) {
	d := startTestDaemonUnix(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "kata"}, nil)

	info, err := daemon.Probe(t.Context(), d.endpoint, daemon.ProbeOptions{ExpectedService: "kata"})
	require.NoError(t, err)
	assert.True(t, info.OK)
}

func TestManagerEnsureStartsAndPollsForCompatibleDaemon(t *testing.T) {
	assert := assert.New(t)

	d := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "tool", Version: "v2"})
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
				Network:   d.endpoint.Network,
				Address:   d.endpoint.Address,
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
	d := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "tool", Version: "old"})
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	storedRecord(t, store, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   d.endpoint.Network,
		Address:   d.endpoint.Address,
		Service:   "tool",
		Version:   "old",
		StartedAt: time.Now(),
	})
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

	ep := closedEndpoint(t)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	storedRecord(t, store, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   ep.Network,
		Address:   ep.Address,
		Service:   "tool",
		StartedAt: time.Now(),
	})

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

	_, _, err := manager.Ensure(t.Context(), time.Second)
	require.Error(err)
	require.ErrorIs(err, daemon.ErrDaemonUnreachable)
	assert.False(started)
}

func TestManagerFindScansPastIncompatibleDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	oldDaemon := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "tool", Version: "old"})
	newDaemon := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid() + 1, Service: "tool", Version: "new"})

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	storedRecord(t, store, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   oldDaemon.endpoint.Network,
		Address:   oldDaemon.endpoint.Address,
		Service:   "tool",
		Version:   "old",
		StartedAt: time.Now().Add(-time.Minute),
	})
	storedRecord(t, store, daemon.RuntimeRecord{
		PID:       os.Getpid() + 1,
		Network:   newDaemon.endpoint.Network,
		Address:   newDaemon.endpoint.Address,
		Service:   "tool",
		Version:   "new",
		StartedAt: time.Now(),
	})

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
	assert.Equal(newDaemon.endpoint.Address, rec.Address)
	assert.Equal("new", info.Version)
}

func TestManagerFindUsesCustomDiscovery(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	d := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "tool", Version: "v1"})
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	storedRecord(t, store, daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   d.endpoint.Network,
		Address:   d.endpoint.Address,
		Service:   "tool",
		Version:   "v1",
		StartedAt: time.Now(),
	})

	calls := 0
	manager := daemon.Manager{
		Store: store,
		FindFunc: func(ctx context.Context) (daemon.RuntimeRecord, daemon.PingInfo, bool, error) {
			calls++
			return daemon.Discover(ctx, store, daemon.DiscoverOptions{
				Probe: daemon.ProbeOptions{ExpectedService: "tool"},
			})
		},
	}
	rec, info, ok, err := manager.Find(t.Context())
	require.NoError(err)
	require.True(ok)
	assert.Equal(d.endpoint.Address, rec.Address)
	assert.Equal("tool", info.Service)
	assert.Equal(1, calls)
}

func TestManagerEnsureSerializesConcurrentStarts(t *testing.T) {
	d := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "tool", Version: "v1"})
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
				Network:   d.endpoint.Network,
				Address:   d.endpoint.Address,
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
		require.NoError(t, <-errs)
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

	d := startTestDaemon(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "tool", Version: "v1"})
	discoveryStore := daemon.RuntimeStore{Dir: t.TempDir()}
	lockStore := daemon.RuntimeStore{Dir: t.TempDir()}

	var starts atomic.Int32
	manager := daemon.Manager{
		Store: lockStore,
		FindFunc: func(ctx context.Context) (daemon.RuntimeRecord, daemon.PingInfo, bool, error) {
			return daemon.Discover(ctx, discoveryStore, daemon.DiscoverOptions{
				Probe: daemon.ProbeOptions{ExpectedService: "tool"},
			})
		},
		Start: func(context.Context) error {
			if starts.Add(1) > 1 {
				return errors.New("start must run at most once")
			}
			time.Sleep(20 * time.Millisecond) //nolint:kennlint // lets the second caller contend on the start lock
			_, err := discoveryStore.Write(daemon.RuntimeRecord{
				PID:       os.Getpid(),
				Network:   d.endpoint.Network,
				Address:   d.endpoint.Address,
				Service:   "tool",
				Version:   "v1",
				StartedAt: time.Now(),
			})
			return err
		},
	}

	results := make(chan error, 2)
	for range 2 {
		go func() {
			_, _, err := manager.Ensure(t.Context(), 2*time.Second)
			results <- err
		}()
	}
	for range 2 {
		require.NoError(<-results)
	}
	assert.Equal(int32(1), starts.Load())
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

func newDaemonProof(t *testing.T, key []byte) *daemon.Proof {
	t.Helper()
	proof, err := daemon.NewProof(key)
	require.NoError(t, err)
	return proof
}
