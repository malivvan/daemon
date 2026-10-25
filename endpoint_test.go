package daemon_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/malivvan/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseEndpointDefaultTCP(t *testing.T) {
	assert := assert.New(t)

	ep, err := daemon.ParseEndpoint("", daemon.ParseEndpointOptions{
		DefaultTCPAddress: "127.0.0.1:7373",
		TCPPolicy:         daemon.RequireLoopback,
	})
	require.NoError(t, err)
	assert.Equal(daemon.NetworkTCP, ep.Network)
	assert.Equal("127.0.0.1:7373", ep.Address)
	assert.Equal("http://127.0.0.1:7373", ep.BaseURL())
	assert.Equal(7373, ep.Port())
}

func TestParseEndpointRejectsPublicTCPWhenPolicyRequiresNonPublic(t *testing.T) {
	_, err := daemon.ParseEndpoint("8.8.8.8:80", daemon.ParseEndpointOptions{
		TCPPolicy: daemon.RequireNonPublic,
	})
	require.Error(t, err)
}

func TestRequireNonPublicAllowsCGNAT(t *testing.T) {
	require.NoError(t, daemon.RequireNonPublic("100.64.0.1:7777"))
}

func TestRequireNonPublicAllowsLocalhost(t *testing.T) {
	require.NoError(t, daemon.RequireNonPublic("localhost:7777"))
}

func TestParseEndpointUnixDefault(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "daemon.sock")
	ep, err := daemon.ParseEndpoint("unix://", daemon.ParseEndpointOptions{
		DefaultUnixPath: socketPath,
	})
	require.NoError(t, err)
	assert.True(t, ep.IsUnix())
	assert.Equal(t, "unix://"+socketPath, ep.ConfigAddress())
}

func TestUnixDialerPerformsHandshake(t *testing.T) {
	require := require.New(t)

	endpoint := daemon.Endpoint{Network: daemon.NetworkUnix, Address: unixSocketPath(t)}
	listener, err := net.Listen("unix", endpoint.Address)
	require.NoError(err)

	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{
		Record: daemon.RuntimeRecord{PID: os.Getpid(), Service: "tool"},
	})
	handler := func(ctx context.Context, conn net.Conn) error {
		if err := ping(ctx, conn); err != nil {
			return err
		}
		request := make([]byte, 2)
		if _, err := io.ReadFull(conn, request); err != nil {
			return err
		}
		_, err := conn.Write([]byte("ok"))
		return err
	}
	startTestHandler(t, listener, endpoint, handler)

	conn, err := endpoint.Dialer(daemon.DialerOptions{}).DialContext(t.Context())
	require.NoError(err)
	defer func() { _ = conn.Close() }()

	_, err = conn.Write([]byte("hi"))
	require.NoError(err)
	reply := make([]byte, 2)
	_, err = io.ReadFull(conn, reply)
	require.NoError(err)
	assert.Equal(t, "ok", string(reply))
}

func TestDialerReportsConnectionFailure(t *testing.T) {
	endpoint := closedEndpoint(t)

	_, err := endpoint.Dialer(daemon.DialerOptions{Timeout: 100 * time.Millisecond}).DialContext(t.Context())
	require.Error(t, err)
}

func TestDefaultSocketPathCreatesPrivateTempFallback(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	service := fmt.Sprintf("kitdtest%d", os.Getpid())
	t.Setenv("TMPDIR", "/tmp")
	t.Setenv("XDG_RUNTIME_DIR", "")

	socketPath := daemon.DefaultSocketPath(service)
	require.NotEmpty(socketPath)
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(socketPath)) })
	assert.Equal(filepath.Join(filepath.Dir(socketPath), "daemon.sock"), socketPath)

	info, err := os.Stat(filepath.Dir(socketPath))
	require.NoError(err)
	assert.True(info.IsDir())
	if runtime.GOOS != "windows" {
		assert.Zero(info.Mode().Perm() & 0o077)
	}
}

func TestDefaultSocketPathRejectsPathLikeServiceNames(t *testing.T) {
	cases := map[string]string{
		"current-directory": ".",
		"parent-directory":  "..",
		"slash":             "bad/service",
		"backslash":         `bad\service`,
		"colon":             "bad:service",
		"space":             "bad service",
		"null":              string([]byte{'b', 0, 'd'}),
	}
	for name, service := range cases {
		t.Run(name, func(t *testing.T) {
			assert.Empty(t, daemon.DefaultSocketPath(service))
		})
	}
}

func TestListenUnixRemovesStaleSocketAndBinds(t *testing.T) {
	socketPath := staleUnixSocket(t)
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}

	listener, err := daemon.Listen(t.Context(), ep)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), daemon.NetworkUnix, socketPath)
	require.NoError(t, err)
	_ = conn.Close()
}

func TestListenUnixRejectsNonSocketPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := unixSocketPath(t)
	require.NoError(os.WriteFile(socketPath, []byte("not a socket"), 0o600))
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}

	listener, err := daemon.Listen(t.Context(), ep)
	require.Error(err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "refusing to remove non-socket path")

	body, readErr := os.ReadFile(socketPath)
	require.NoError(readErr)
	assert.Equal("not a socket", string(body))
}

func TestListenUnixRejectsLiveSocket(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := unixSocketPath(t)
	live, err := (&net.ListenConfig{}).Listen(t.Context(), daemon.NetworkUnix, socketPath)
	require.NoError(err)
	t.Cleanup(func() { _ = live.Close() })
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}

	listener, err := daemon.Listen(t.Context(), ep)
	require.Error(err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "daemon already listening")

	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), daemon.NetworkUnix, socketPath)
	require.NoError(err)
	_ = conn.Close()
}

func TestListenUnixSerializesConcurrentStaleSocketStartup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := staleUnixSocket(t)
	lockPath := filepath.Join(filepath.Dir(socketPath), "daemon.lock")
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}
	opt := daemon.WithListenLockPath(lockPath)

	const starters = 16
	start := make(chan struct{})
	results := make(chan listenResult, starters)
	for range starters {
		go func() {
			<-start
			listener, err := daemon.Listen(t.Context(), ep, opt)
			results <- listenResult{listener: listener, err: err}
		}()
	}
	close(start)

	var winner net.Listener
	var errors []error
	for range starters {
		result := <-results
		if result.err == nil {
			require.Nil(winner, "only one daemon start should bind the socket")
			winner = result.listener
			continue
		}
		errors = append(errors, result.err)
	}
	require.NotNil(winner)
	t.Cleanup(func() { _ = winner.Close() })
	require.Len(errors, starters-1)
	for _, err := range errors {
		assert.Contains(err.Error(), "daemon already listening")
	}

	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), daemon.NetworkUnix, socketPath)
	require.NoError(err)
	_ = conn.Close()
}

func TestListenUnixProbesAfterAcquiringLock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := staleUnixSocket(t)
	lockPath := filepath.Join(filepath.Dir(socketPath), "daemon.lock")
	release, err := daemon.HoldLockForTest(lockPath)
	require.NoError(err)
	locked := true
	t.Cleanup(func() {
		if locked {
			release()
		}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}
	resultCh := make(chan listenResult, 1)
	go func() {
		listener, err := daemon.Listen(ctx, ep, daemon.WithListenLockPath(lockPath))
		resultCh <- listenResult{listener: listener, err: err}
	}()

	require.NoError(os.Remove(socketPath))
	live, err := (&net.ListenConfig{}).Listen(t.Context(), daemon.NetworkUnix, socketPath)
	require.NoError(err)
	t.Cleanup(func() { _ = live.Close() })

	release()
	locked = false
	result := <-resultCh
	require.Error(result.err)
	assert.Nil(result.listener)
	assert.Contains(result.err.Error(), "daemon already listening")

	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), daemon.NetworkUnix, socketPath)
	require.NoError(err)
	_ = conn.Close()
}

type listenResult struct {
	listener net.Listener
	err      error
}

// staleUnixSocket leaves a socket file with no listener, as a daemon that
// exits without closing its listener does.
func staleUnixSocket(t *testing.T) string {
	t.Helper()
	socketPath := unixSocketPath(t)
	listener, err := net.ListenUnix(daemon.NetworkUnix, &net.UnixAddr{Name: socketPath, Net: daemon.NetworkUnix})
	require.NoError(t, err)
	listener.SetUnlinkOnClose(false)
	require.NoError(t, listener.Close())
	_, err = os.Lstat(socketPath)
	require.NoError(t, err, "closed listener did not leave a socket path")
	return socketPath
}

func unixSocketPath(t *testing.T) string {
	t.Helper()
	// Unix socket paths must stay short, so Unix tests need a fixed short temp
	// root; the Windows temp dir is short enough.
	root := "/tmp"
	if runtime.GOOS == "windows" {
		root = ""
	}
	dir, err := os.MkdirTemp(root, "kitd") //nolint:usetesting // see the socket path length note above
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	// Listen requires a private socket dir; a Windows temp dir inherits a
	// broader DACL until restricted.
	require.NoError(t, daemon.EnsurePrivateDirForTest(dir))
	return filepath.Join(dir, "d.sock")
}
