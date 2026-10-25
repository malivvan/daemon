package daemon

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// StartLivePIDHelperForTest starts a child process that lives until test cleanup.
func StartLivePIDHelperForTest(t *testing.T) int {
	t.Helper()
	return startLivePIDHelper(t)
}

// ErrNotPortable exposes the portable-name sentinel to external tests.
var ErrNotPortable = errNotPortable

// EnsurePrivateDirForTest creates a private runtime directory for tests that
// need one without going through another package.
func EnsurePrivateDirForTest(path string) error {
	return ensurePrivateRuntimeDir(path)
}

// HoldLockForTest acquires path's daemon lock, blocking until it is available,
// and returns the release function. It lets tests hold a lock while another
// caller contends for it.
func HoldLockForTest(path string) (func(), error) {
	return acquireDaemonLock(context.Background(), path, "test hold daemon lock")
}

// startLivePIDHelper starts a child process that lives until test cleanup.
func startLivePIDHelper(t *testing.T) int {
	t.Helper()
	require := require.New(t)

	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), os.Args[0], "-test.run=^TestLivePIDHelper$")
	cmd.Env = append(os.Environ(), "KIT_DAEMON_LIVE_PID_HELPER=1")
	stdin, err := cmd.StdinPipe()
	require.NoError(err)
	stdout, err := cmd.StdoutPipe()
	require.NoError(err)
	require.NoError(cmd.Start())
	t.Cleanup(func() {
		require.NoError(stdin.Close())
		require.NoError(cmd.Wait())
	})

	ready, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(err)
	require.Equal("ready\n", ready)
	return cmd.Process.Pid
}

func TestLivePIDHelper(t *testing.T) {
	if os.Getenv("KIT_DAEMON_LIVE_PID_HELPER") != "1" {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestRuntimeRecordBlocksStartForeign(t *testing.T) {
	pid := startLivePIDHelper(t)
	assert.False(t, runtimeRecordBlocksStart(RuntimeRecord{PID: pid}, ProcessScopeForeign))
}

func TestProcessIdentityCompatibilityRejectsValuesOutsideEmissionDomain(t *testing.T) {
	assert.True(t, processIdentityCompatible(ProcessIdentity(strconv.FormatInt(math.MaxInt64, 10))))
	assert.False(t, processIdentityCompatible("9223372036854775808"))
}

func TestRuntimeStoreWriteCheckStaysOutsideDiscoveryNamespaces(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := RuntimeStore{Dir: t.TempDir(), Prefix: "tool"}
	prefix, err := store.validatePrefix()
	require.NoError(err)
	probe, err := os.CreateTemp(store.Dir, fmt.Sprintf(runtimeWriteCheckPattern, prefix))
	require.NoError(err)
	t.Cleanup(func() {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
	})

	records, err := store.List()
	require.NoError(err)
	assert.Empty(records)
	name := filepath.Base(probe.Name())
	_, isRecord := pidFromName(prefix, name)
	assert.False(isRecord)
	assert.NotEqual(prefix+".lock", name)
	assert.NotEqual(prefix+".listen.lock", name)
}

func TestRuntimeStoreWriteReturnsPathWhenRecordWasPublishedButNotDurable(t *testing.T) {
	require := require.New(t)
	store := RuntimeStore{Dir: t.TempDir()}
	original := writeAtomicFile
	writeAtomicFile = func(path string, data []byte, perm os.FileMode) error {
		if err := original(path, data, perm); err != nil {
			return err
		}
		return errors.Join(errNotDurable, errors.New("injected directory sync failure"))
	}
	t.Cleanup(func() { writeAtomicFile = original })

	path, err := store.Write(RuntimeRecord{PID: 1, Address: "127.0.0.1:7474"})

	require.ErrorIs(err, errNotDurable)
	assert.Equal(t, filepath.Join(store.Dir, "daemon.1.json"), path)
	_, statErr := os.Stat(path)
	require.NoError(statErr)
}

func TestProofMACBindsRuntimeIdentity(t *testing.T) {
	key := []byte("identity-proof-key")
	nonce := bytes.Repeat([]byte{0x42}, proofNonceSize)
	rec := RuntimeRecord{
		PID:     123,
		Network: NetworkTCP,
		Address: "127.0.0.1:4321",
		Service: "tool",
		Version: "v1",
	}
	want, err := proofMAC(key, nonce, rec)
	require.NoError(t, err)

	tests := []struct {
		name string
		rec  RuntimeRecord
	}{
		{name: "service", rec: RuntimeRecord{PID: 123, Network: NetworkTCP, Address: "127.0.0.1:4321", Service: "other", Version: "v1"}},
		{name: "version", rec: RuntimeRecord{PID: 123, Network: NetworkTCP, Address: "127.0.0.1:4321", Service: "tool", Version: "v2"}},
		{name: "PID", rec: RuntimeRecord{PID: 456, Network: NetworkTCP, Address: "127.0.0.1:4321", Service: "tool", Version: "v1"}},
		{name: "network", rec: RuntimeRecord{PID: 123, Network: NetworkUnix, Address: "127.0.0.1:4321", Service: "tool", Version: "v1"}},
		{name: "address", rec: RuntimeRecord{PID: 123, Network: NetworkTCP, Address: "127.0.0.1:9876", Service: "tool", Version: "v1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert := assert.New(t)
			require := require.New(t)

			got, err := proofMAC(key, nonce, tt.rec)
			require.NoError(err)
			assert.NotEqual(want, got)
		})
	}
}

func TestProofPingHandlerRejectsMalformedChallenge(t *testing.T) {
	require := require.New(t)

	proof, err := NewProof([]byte("handler-proof-key"))
	require.NoError(err)
	handler, err := proof.NewPingHandler(RuntimeRecord{
		PID:     123,
		Network: NetworkTCP,
		Address: "127.0.0.1:4321",
		Service: "tool",
	})
	require.NoError(err)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, DefaultPingPath, nil)
	req.Header.Set(proofChallengeHeader, "not-a-valid-challenge")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusBadRequest, recorder.Code)
	assert.Empty(t, recorder.Header().Get(proofResponseHeader))
}

func TestProofProbeRejectsMalformedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set(proofResponseHeader, "not-a-valid-proof")
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"v1","pid":123}`)
	}))
	defer server.Close()

	rec := RuntimeRecord{
		PID:     123,
		Network: NetworkTCP,
		Address: server.Listener.Addr().String(),
		Service: "tool",
		Version: "v1",
	}
	proof, err := NewProof([]byte("client-proof-key"))
	require.NoError(t, err)
	_, err = proof.probeHTTP(t.Context(), server.Client(), server.URL, rec, ProbeOptions{
		ExpectedService: "tool",
	})
	require.Error(t, err)
}

func TestProofProbeRejectsReplayedResponse(t *testing.T) {
	require := require.New(t)

	key := []byte("replay-proof-key")
	serverProof, err := NewProof(key)
	require.NoError(err)
	server := httptest.NewUnstartedServer(nil)
	rec := RuntimeRecord{
		PID:     123,
		Network: NetworkTCP,
		Address: server.Listener.Addr().String(),
		Service: "tool",
		Version: "v1",
	}
	handler, err := serverProof.NewPingHandler(rec)
	require.NoError(err)
	server.Config.Handler = handler
	server.Start()
	defer server.Close()

	client := *server.Client()
	client.Transport = &replayProofTransport{base: client.Transport}
	clientProof, err := NewProof(key)
	require.NoError(err)
	_, err = clientProof.probeHTTP(t.Context(), &client, server.URL, rec, ProbeOptions{
		ExpectedService: "tool",
	})
	require.NoError(err)
	_, err = clientProof.probeHTTP(t.Context(), &client, server.URL, rec, ProbeOptions{
		ExpectedService: "tool",
	})
	require.Error(err)
}

type replayProofTransport struct {
	base         http.RoundTripper
	firstHeaders http.Header
}

func (t *replayProofTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if t.firstHeaders == nil {
		t.firstHeaders = resp.Header.Clone()
	} else {
		resp.Header = t.firstHeaders.Clone()
	}
	return resp, nil
}
