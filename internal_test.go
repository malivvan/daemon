package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
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

func testProof(t *testing.T, key string) *Proof {
	t.Helper()
	proof, err := NewProof([]byte(key))
	require.NoError(t, err)
	return proof
}

func testPingRecord() RuntimeRecord {
	return RuntimeRecord{
		PID:     123,
		Network: NetworkTCP,
		Address: "127.0.0.1:4321",
		Service: "tool",
		Version: "v1",
	}
}

func TestPingRequestEncodeDecode(t *testing.T) {
	challenge := bytes.Repeat([]byte{0x42}, proofNonceSize)
	encoded, err := (PingRequest{Challenge: challenge}).Encode()
	require.NoError(t, err)

	var decoded PingRequest
	require.NoError(t, decoded.Decode(encoded))
	assert.Equal(t, challenge, decoded.Challenge)

	empty, err := (PingRequest{}).Encode()
	require.NoError(t, err)
	var emptyDecoded PingRequest
	require.NoError(t, emptyDecoded.Decode(empty))
	assert.Empty(t, emptyDecoded.Challenge)
}

func TestPingResponseEncodeDecode(t *testing.T) {
	want := PingResponse{
		Info:  PingInfo{OK: true, Service: "tool", Version: "v1", PID: 123},
		Proof: bytes.Repeat([]byte{0x24}, 32),
		Error: "",
	}
	encoded, err := want.Encode()
	require.NoError(t, err)

	var decoded PingResponse
	require.NoError(t, decoded.Decode(encoded))
	assert.Equal(t, want.Info, decoded.Info)
	assert.Equal(t, want.Proof, decoded.Proof)
	assert.Empty(t, decoded.Error)
}

func TestPingMessageDecodeRejectsMalformedInput(t *testing.T) {
	valid, err := (PingResponse{Info: PingInfo{OK: true, Service: "tool", PID: 1}}).Encode()
	require.NoError(t, err)

	tests := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "truncated", data: valid[:len(valid)-1]},
		{name: "trailing", data: append(bytes.Clone(valid), 0)},
		{name: "wrong type", data: append([]byte{99}, valid[1:]...)},
		{name: "wrong version", data: append([]byte{valid[0], 2}, valid[2:]...)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var decoded PingResponse
			require.Error(t, decoded.Decode(tt.data))
		})
	}
}

func TestReadMessageRejectsOversizedFrame(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	go func() {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], maxMessageSize+1)
		_, _ = server.Write(size[:])
	}()

	_, err := readMessage(client)
	require.Error(t, err)
}

func TestWireStringsAreLengthBounded(t *testing.T) {
	_, err := (PingResponse{Info: PingInfo{Service: string(bytes.Repeat([]byte{'a'}, 0x10000))}}).Encode()
	require.Error(t, err)
}

func TestProofMACBindsRuntimeIdentity(t *testing.T) {
	key := []byte("identity-proof-key")
	nonce := bytes.Repeat([]byte{0x42}, proofNonceSize)
	endpoint := Endpoint{Network: NetworkTCP, Address: "127.0.0.1:4321"}
	info := PingInfo{OK: true, Service: "tool", Version: "v1", PID: 123}
	want, err := proofMAC(key, nonce, endpoint, info)
	require.NoError(t, err)

	tests := []struct {
		name     string
		endpoint Endpoint
		info     PingInfo
	}{
		{name: "service", endpoint: endpoint, info: PingInfo{OK: true, Service: "other", Version: "v1", PID: 123}},
		{name: "version", endpoint: endpoint, info: PingInfo{OK: true, Service: "tool", Version: "v2", PID: 123}},
		{name: "PID", endpoint: endpoint, info: PingInfo{OK: true, Service: "tool", Version: "v1", PID: 456}},
		{name: "network", endpoint: Endpoint{Network: NetworkUnix, Address: "127.0.0.1:4321"}, info: info},
		{name: "address", endpoint: Endpoint{Network: NetworkTCP, Address: "127.0.0.1:9876"}, info: info},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := proofMAC(key, nonce, tt.endpoint, tt.info)
			require.NoError(t, err)
			assert.NotEqual(t, want, got)
		})
	}
}

func TestPingHandlerRejectsMalformedChallenge(t *testing.T) {
	endpoint := testPingRecord()
	handler := NewPingHandler(PingHandlerOptions{Record: endpoint, Proof: testProof(t, "handler-proof-key")})
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	done := make(chan error, 1)
	go func() {
		err := handler(t.Context(), server)
		_ = server.Close()
		done <- err
	}()

	require.NoError(t, writeMessage(client, PingRequest{Challenge: []byte{1}}))
	payload, err := readMessage(client)
	require.NoError(t, err)
	var resp PingResponse
	require.NoError(t, resp.Decode(payload))
	assert.False(t, resp.Info.OK)
	assert.Contains(t, resp.Error, "invalid daemon proof challenge")
	require.NoError(t, <-done)
}

func TestPingHandlerWithoutProofRejectsChallenge(t *testing.T) {
	handler := NewPingHandler(PingHandlerOptions{Record: testPingRecord()})
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	done := make(chan error, 1)
	go func() {
		err := handler(t.Context(), server)
		_ = server.Close()
		done <- err
	}()

	require.NoError(t, writeMessage(client, PingRequest{Challenge: bytes.Repeat([]byte{0x11}, proofNonceSize)}))
	payload, err := readMessage(client)
	require.NoError(t, err)
	var resp PingResponse
	require.NoError(t, resp.Decode(payload))
	assert.False(t, resp.Info.OK)
	assert.Contains(t, resp.Error, "proof is not configured")
	require.NoError(t, <-done)
}

func TestPingHandlerAnswersProofChallenge(t *testing.T) {
	rec := testPingRecord()
	proof := testProof(t, "handler-proof-key")
	handler := NewPingHandler(PingHandlerOptions{Record: rec, Proof: proof})
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	done := make(chan error, 1)
	go func() {
		err := handler(t.Context(), server)
		_ = server.Close()
		done <- err
	}()

	challenge := bytes.Repeat([]byte{0x33}, proofNonceSize)
	require.NoError(t, writeMessage(client, PingRequest{Challenge: challenge}))
	payload, err := readMessage(client)
	require.NoError(t, err)
	var resp PingResponse
	require.NoError(t, resp.Decode(payload))
	assert.True(t, resp.Info.OK)

	want, err := proofMAC(proof.key, challenge, rec.Endpoint(), resp.Info)
	require.NoError(t, err)
	assert.Equal(t, want, resp.Proof)
	require.NoError(t, <-done)
}

func TestPingConnRejectsMalformedProof(t *testing.T) {
	endpoint := Endpoint{Network: NetworkTCP, Address: "127.0.0.1:4321"}
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	go func() {
		_, _ = readMessage(server)
		_ = writeMessage(server, PingResponse{
			Info:  PingInfo{OK: true, Service: "tool", Version: "v1", PID: 123},
			Proof: []byte("short"),
		})
		_ = server.Close()
	}()

	_, err := pingConn(t.Context(), client, endpoint, testProof(t, "client-proof-key"))
	require.Error(t, err)
}

func TestPingConnRejectsNonOKResponse(t *testing.T) {
	endpoint := Endpoint{Network: NetworkTCP, Address: "127.0.0.1:4321"}
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	go func() {
		_, _ = readMessage(server)
		_ = writeMessage(server, PingResponse{Info: PingInfo{OK: false}})
		_ = server.Close()
	}()

	_, err := pingConn(t.Context(), client, endpoint, nil)
	require.Error(t, err)
}

func TestPingConnRejectsReplayedProof(t *testing.T) {
	endpoint := Endpoint{Network: NetworkTCP, Address: "127.0.0.1:4321"}
	info := PingInfo{OK: true, Service: "tool", Version: "v1", PID: 123}
	key := []byte("replay-proof-key")
	var replayed []byte

	run := func() error {
		client, server := net.Pipe()
		defer func() { _ = client.Close() }()
		go func() {
			payload, err := readMessage(server)
			if err == nil {
				var req PingRequest
				if err == nil {
					err = req.Decode(payload)
				}
				if err == nil {
					if replayed == nil {
						replayed, err = proofMAC(key, req.Challenge, endpoint, info)
					}
					if err == nil {
						_ = writeMessage(server, PingResponse{Info: info, Proof: replayed})
					}
				}
			}
			_ = server.Close()
		}()
		_, err := pingConn(t.Context(), client, endpoint, testProof(t, string(key)))
		return err
	}

	require.NoError(t, run())
	require.Error(t, run())
}

func TestPingConnVerifiesResponseIdentity(t *testing.T) {
	endpoint := Endpoint{Network: NetworkTCP, Address: "127.0.0.1:4321"}
	key := []byte("identity-bind-key")
	serverInfo := PingInfo{OK: true, Service: "tool", Version: "v1", PID: 999}
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()

	go func() {
		payload, _ := readMessage(server)
		var req PingRequest
		_ = req.Decode(payload)
		mac, _ := proofMAC(key, req.Challenge, endpoint, serverInfo)
		_ = writeMessage(server, PingResponse{Info: serverInfo, Proof: mac})
		_ = server.Close()
	}()

	info, err := pingConn(t.Context(), client, endpoint, testProof(t, string(key)))
	require.NoError(t, err)
	assert.Equal(t, 999, info.PID)
}
