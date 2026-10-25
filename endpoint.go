// Package daemon provides reusable infrastructure for local background
// daemons used by CLI tools.
//
// It covers the lifecycle pieces that should be shared across tools:
// endpoint parsing and HTTP clients for TCP or Unix sockets, atomic
// daemon.<pid>.json runtime files with opaque process-creation identity,
// distinct start and lifetime-owner locks, PID plus ping-based discovery, and
// a small Manager that can auto-start an application daemon through a
// caller-provided StartFunc. Process creation identity is optional when the
// platform cannot inspect it and an unknown identity never proves ownership.
// Manager uses runtime-file discovery by default and accepts a FindFunc when
// callers need custom probing while retaining its start locking, re-discovery,
// polling, and timeout behavior.
//
// ProcessAlive and Kill expose platform liveness and immediate-termination
// primitives for callers that must end a daemon that no longer answers.
//
// RuntimeStore.TryAcquireStartLock supports nonblocking startup markers and
// probes. A successful caller holds the same lock as Manager.Ensure until it
// invokes the release function. Callers may inspect and clean up their own
// startup state before releasing. Contention includes holders in this process;
// callers that need to recognize their own marker must synchronize acquisition,
// registration of the release function, probing, and release themselves.
// Acquisition errors leave the state unknown. A caller may retry after resolving
// an error, but only successful acquisition authorizes cleanup or launch.
// Snapshot age, process identity checks, and retry policy belong to the caller.
// A released probe grants no reservation for a subsequent launch.
//
// Authenticated discovery must prove an endpoint before any bearer credential
// is sent. Servers construct a Proof from the shared daemon token and register
// its NewPingHandler result using the same RuntimeRecord they write to
// RuntimeStore. Clients construct their own Proof from that token, place it in
// DiscoverOptions.Proof, and wait for Manager.Find or Manager.Ensure to succeed
// before using a credential-bearing client. The package sends a random
// challenge and verifies an identity-bound HMAC; it does not send or expose the
// token. Custom FindFunc callbacks must preserve this ordering and can use
// Proof.Probe directly.
//
// Stop verifies a recorded endpoint, runs the caller's shutdown request, and
// waits for exit with a timeout. StopOptions.Proof is required for TCP records
// in every process scope; StopOptions.Exited alone decides exit when set.
//
// Application-specific server setup, database wiring, auth policy, CLI
// commands, and shutdown behavior remain in the owning tool.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// NetworkTCP identifies TCP endpoints.
	NetworkTCP = "tcp"
	// NetworkUnix identifies Unix-domain socket endpoints.
	NetworkUnix = "unix"

	defaultStaleSocketProbeTimeout = 500 * time.Millisecond
)

// MaxUnixPathLen is the platform socket path length limit.
// macOS/BSD: 104, Linux: 108.
var MaxUnixPathLen = func() int {
	switch runtime.GOOS {
	case "darwin", "dragonfly", "freebsd", "netbsd", "openbsd":
		return 104
	}
	return 108
}()

// Endpoint describes where a daemon listens and how clients should dial it.
type Endpoint struct {
	Network string `json:"network"`
	Address string `json:"address"`
}

// TCPAddressPolicy validates TCP host:port strings while parsing endpoints.
type TCPAddressPolicy func(addr string) error

// ParseEndpointOptions configures ParseEndpoint.
type ParseEndpointOptions struct {
	// DefaultTCPAddress is used when the raw address is empty and no
	// DefaultUnixPath is set.
	DefaultTCPAddress string
	// DefaultUnixPath is used for an empty raw address or raw "unix://".
	DefaultUnixPath string
	// TCPPolicy validates TCP endpoints. Nil accepts any syntactically valid
	// TCP host:port.
	TCPPolicy TCPAddressPolicy
}

// ParseEndpoint parses "host:port", "http://host:port", or "unix:///path".
func ParseEndpoint(raw string, opts ParseEndpointOptions) (Endpoint, error) {
	if raw == "" {
		if opts.DefaultUnixPath != "" {
			return parseUnixEndpoint(opts.DefaultUnixPath)
		}
		raw = opts.DefaultTCPAddress
	}
	if raw == "" {
		return Endpoint{}, errors.New("empty daemon address")
	}
	if after, ok := strings.CutPrefix(raw, "http://"); ok {
		raw = after
	}
	if after, ok := strings.CutPrefix(raw, "unix://"); ok {
		if after == "" {
			after = opts.DefaultUnixPath
		}
		return parseUnixEndpoint(after)
	}
	return parseTCPEndpoint(raw, opts.TCPPolicy)
}

func parseTCPEndpoint(addr string, policy TCPAddressPolicy) (Endpoint, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return Endpoint{}, fmt.Errorf("invalid tcp daemon address %q: %w", addr, err)
	}
	if policy != nil {
		if err := policy(addr); err != nil {
			return Endpoint{}, err
		}
	}
	return Endpoint{Network: NetworkTCP, Address: addr}, nil
}

func parseUnixEndpoint(path string) (Endpoint, error) {
	if path == "" {
		return Endpoint{}, errors.New("empty unix socket path")
	}
	if !filepath.IsAbs(path) {
		return Endpoint{}, fmt.Errorf("unix socket path %q must be absolute", path)
	}
	if strings.ContainsRune(path, 0) {
		return Endpoint{}, errors.New("unix socket path contains null byte")
	}
	if len(path) >= MaxUnixPathLen {
		return Endpoint{}, fmt.Errorf(
			"unix socket path %q (%d bytes) exceeds platform limit of %d bytes",
			path, len(path), MaxUnixPathLen)
	}
	return Endpoint{Network: NetworkUnix, Address: path}, nil
}

// RequireLoopback rejects any TCP address whose host is not loopback.
func RequireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse host:port: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("address %q is not a literal IP or localhost", addr)
	}
	if !ip.IsLoopback() {
		return fmt.Errorf("address %q is not loopback", addr)
	}
	return nil
}

// RequireNonPublic rejects public TCP addresses and unspecified binds.
func RequireNonPublic(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("parse host:port: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("address %q is not a literal IP", addr)
	}
	if ip.IsUnspecified() {
		return fmt.Errorf("address %q is unspecified; use a private address", addr)
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnatBlock.Contains(ip) {
		return nil
	}
	return fmt.Errorf("address %q is public; use a private address", addr)
}

var cgnatBlock = &net.IPNet{
	IP:   net.IPv4(100, 64, 0, 0).To4(),
	Mask: net.CIDRMask(10, 32),
}

// IsUnix reports whether e uses a Unix-domain socket.
func (e Endpoint) IsUnix() bool {
	return e.Network == NetworkUnix
}

// ConfigAddress returns a stable address string for config/runtime files.
func (e Endpoint) ConfigAddress() string {
	if e.IsUnix() {
		return "unix://" + e.Address
	}
	return e.Address
}

// BaseURL returns the HTTP base URL for daemon API requests.
func (e Endpoint) BaseURL() string {
	if e.IsUnix() {
		return "http://localhost"
	}
	return "http://" + e.Address
}

// Port returns the TCP port, or 0 for non-TCP endpoints.
func (e Endpoint) Port() int {
	if e.IsUnix() {
		return 0
	}
	_, portText, err := net.SplitHostPort(e.Address)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return 0
	}
	return port
}

// Listen binds a listener for the endpoint without a setup deadline. Prefer
// ListenContext when a context is available.
func (e Endpoint) Listen() (net.Listener, error) {
	return e.ListenContext(context.Background())
}

// ListenContext binds a listener for the endpoint. The context bounds
// listener setup only; it does not close the returned listener.
func (e Endpoint) ListenContext(ctx context.Context) (net.Listener, error) {
	if e.Network == "" {
		return nil, errors.New("empty daemon endpoint network")
	}
	if e.Address == "" {
		return nil, errors.New("empty daemon endpoint address")
	}
	return (&net.ListenConfig{}).Listen(ctx, e.Network, e.Address)
}

// HTTPClientOptions configures HTTPClient.
type HTTPClientOptions struct {
	Timeout               time.Duration
	ResponseHeaderTimeout time.Duration
	DisableKeepAlives     bool
}

// HTTPClient returns a client configured to dial the endpoint.
func (e Endpoint) HTTPClient(opts HTTPClientOptions) *http.Client {
	client := &http.Client{Timeout: opts.Timeout}
	if e.IsUnix() {
		client.Transport = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, NetworkUnix, e.Address)
			},
			DisableKeepAlives:     opts.DisableKeepAlives,
			Proxy:                 nil,
			ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		}
		return client
	}
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		client.Transport = &http.Transport{Proxy: nil}
		return client
	}
	transport := base.Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = opts.ResponseHeaderTimeout
	transport.DisableKeepAlives = opts.DisableKeepAlives
	client.Transport = transport
	return client
}

// DefaultSocketPath returns a per-user socket path under XDG_RUNTIME_DIR when
// available, otherwise under a private directory in os.TempDir(). It returns
// an empty string when service is not a single path component or no private
// socket path can be prepared within the platform path limit.
func DefaultSocketPath(service string) string {
	if service == "" {
		service = "daemon"
	}
	if !validSocketService(service) {
		return ""
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" && filepath.IsAbs(xdg) {
		if info, err := os.Stat(xdg); err == nil && info.IsDir() {
			parent := filepath.Join(xdg, service)
			if err := ensurePrivateRuntimeDir(parent); err == nil {
				path := filepath.Join(parent, "daemon.sock")
				if len(path) < MaxUnixPathLen {
					return path
				}
			}
		}
	}
	parent := filepath.Join(os.TempDir(), fmt.Sprintf("%s-%s", service, runtimeUID()))
	if err := ensurePrivateRuntimeDir(parent); err != nil {
		return ""
	}
	path := filepath.Join(parent, "daemon.sock")
	if len(path) >= MaxUnixPathLen {
		return ""
	}
	return path
}

func validSocketService(service string) bool {
	if service == "" || service == "." || service == ".." || strings.ContainsRune(service, 0) {
		return false
	}
	if filepath.Base(service) != service {
		return false
	}
	if strings.ContainsAny(service, `/\`) {
		return false
	}
	for _, r := range service {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			continue
		}
		if r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

type listenOptions struct {
	// Store provides the daemon runtime listen lock used to serialize Unix
	// socket stale cleanup and bind. Ignored when LockPath is set.
	store RuntimeStore

	// LockPath overrides the lock file used for Unix socket startup.
	lockPath string

	// StaleSocketProbeTimeout bounds the local dial used to prove that an
	// existing Unix socket path is stale before removing it.
	staleSocketProbeTimeout time.Duration
}

// ListenOption configures Listen.
type ListenOption func(*listenOptions)

// WithRuntimeStore uses store's daemon listen lock to serialize Unix socket
// stale cleanup and bind. Ignored when WithListenLockPath is also supplied.
func WithRuntimeStore(store RuntimeStore) ListenOption {
	return func(opts *listenOptions) {
		opts.store = store
	}
}

// WithListenLockPath overrides the lock file used for Unix socket startup.
func WithListenLockPath(lockPath string) ListenOption {
	return func(opts *listenOptions) {
		opts.lockPath = lockPath
	}
}

// WithStaleSocketProbeTimeout bounds the local dial used to prove that an
// existing Unix socket path is stale before removing it.
func WithStaleSocketProbeTimeout(timeout time.Duration) ListenOption {
	return func(opts *listenOptions) {
		opts.staleSocketProbeTimeout = timeout
	}
}

// Listen binds ep for daemon serving.
//
// For Unix sockets, Listen serializes stale socket probing/removal and the
// subsequent bind under an inter-process lock. Existing live sockets and
// non-socket paths are rejected. TCP endpoints retain Endpoint's normal Listen
// behavior.
func Listen(ctx context.Context, ep Endpoint, options ...ListenOption) (net.Listener, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	opts := listenOptions{}
	for _, option := range options {
		option(&opts)
	}
	if !ep.IsUnix() {
		return ep.ListenContext(ctx)
	}
	if err := prepareUnixListenEndpoint(ep); err != nil {
		return nil, err
	}
	lockPath, err := opts.listenLockPath(ep)
	if err != nil {
		return nil, err
	}
	unlock, err := acquireDaemonLock(ctx, lockPath, "acquire daemon listen lock")
	if err != nil {
		return nil, err
	}
	defer unlock()
	if err := removeStaleUnixSocket(ctx, ep, opts); err != nil {
		return nil, err
	}
	return ep.ListenContext(ctx)
}

func prepareUnixListenEndpoint(ep Endpoint) error {
	if ep.Address == "" {
		return errors.New("empty daemon endpoint address")
	}
	if !filepath.IsAbs(ep.Address) {
		return fmt.Errorf("unix socket path %q must be absolute", ep.Address)
	}
	if err := validatePrivateRuntimeDir(filepath.Dir(ep.Address)); err != nil {
		return fmt.Errorf("validate unix socket dir: %w", err)
	}
	return nil
}

func (opts listenOptions) listenLockPath(ep Endpoint) (string, error) {
	if opts.lockPath != "" {
		return opts.lockPath, nil
	}
	if opts.store.Dir != "" {
		return opts.store.ListenLockPath()
	}
	if ep.Address == "" {
		return "", errors.New("empty daemon endpoint address")
	}
	return ep.Address + ".lock", nil
}

func (opts listenOptions) staleProbeTimeout() time.Duration {
	if opts.staleSocketProbeTimeout > 0 {
		return opts.staleSocketProbeTimeout
	}
	return defaultStaleSocketProbeTimeout
}

func removeStaleUnixSocket(ctx context.Context, ep Endpoint, opts listenOptions) error {
	if ep.Address == "" {
		return errors.New("empty daemon endpoint address")
	}
	info, err := os.Lstat(ep.Address)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect unix socket %s: %w", ep.Address, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket path %s", ep.Address)
	}
	stale, err := unixSocketStale(ctx, ep.Address, opts.staleProbeTimeout())
	if err != nil {
		return err
	}
	if !stale {
		return fmt.Errorf("daemon already listening on unix socket %s", ep.Address)
	}
	if err := os.Remove(ep.Address); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove stale unix socket %s: %w", ep.Address, err)
	}
	return nil
}

func unixSocketStale(ctx context.Context, path string, timeout time.Duration) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(probeCtx, NetworkUnix, path)
	if err == nil {
		_ = conn.Close()
		return false, nil
	}
	if ctxErr := probeCtx.Err(); ctxErr != nil {
		return false, fmt.Errorf("probe unix socket %s: %w", path, ctxErr)
	}
	if isStaleUnixSocketDialError(err) {
		return true, nil
	}
	return false, fmt.Errorf("probe unix socket %s: %w", path, err)
}
