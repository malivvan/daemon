# daemon

`daemon` is a small Go package for building **local background daemons** and the
**CLI clients that talk to them**.

It does not impose an application protocol, server framework, database, config
format, auth policy, or shutdown policy. Instead it provides the parts that are
easy to get subtly wrong:

- endpoint parsing and **dialers** for TCP and Unix-domain sockets;
- a small **length-prefixed binary handshake** used for liveness probes,
  discovery, and optional proof-of-possession;
- atomic **runtime records** (`<prefix>.<pid>.json`) with opaque
  **process-creation identity**;
- PID-aware **discovery**, optional proof, and a `Manager` that can
  **auto-start** a daemon through a caller-supplied `StartFunc`;
- **three distinct locks**: a start lock, a lifetime owner lock, and a listen
  lock for Unix-socket startup;
- a verified **`Stop`** that runs the caller's shutdown request and waits for
  exit;
- platform liveness and termination primitives (`ProcessAlive`, `Kill`).

The package has **no dependency on `net/http`** and no runtime dependencies
outside the standard library. Applications own the protocol that follows the
daemon handshake.

A PID alone never proves that a recorded daemon is still the same process. An
identity that cannot be judged never authorizes deleting a record or signaling
a process.

---

## Contents

- [Features](#features)
- [Platform support](#platform-support)
- [Requirements and installation](#requirements-and-installation)
- [Quick start: a complete daemon and client](#quick-start-a-complete-daemon-and-client)
- [Core concepts](#core-concepts)
  - [Endpoints and dialers](#endpoints-and-dialers)
  - [The daemon handshake](#the-daemon-handshake)
  - [Runtime records](#runtime-records)
  - [Process identity and liveness](#process-identity-and-liveness)
  - [Locks](#locks)
  - [Proof of possession](#proof-of-possession)
  - [Stopping a daemon](#stopping-a-daemon)
- [Usage guide](#usage-guide)
  - [Parsing endpoints and dialing](#parsing-endpoints-and-dialing)
  - [Serving a daemon](#serving-a-daemon)
  - [Probing and discovery](#probing-and-discovery)
  - [Auto-starting with Manager.Ensure](#auto-starting-with-managerensure)
  - [Authenticated daemons with Proof](#authenticated-daemons-with-proof)
  - [Stopping a daemon](#stopping-a-daemon-1)
  - [Runtime-store housekeeping](#runtime-store-housekeeping)
  - [Locks in practice](#locks-in-practice)
  - [Process controls and identity](#process-controls-and-identity)
  - [Starting a detached child](#starting-a-detached-child)
- [API reference](#api-reference)
- [Error reference](#error-reference)
- [Security notes](#security-notes)
- [Platform notes](#platform-notes)
- [Development and testing](#development-and-testing)
- [Package layout](#package-layout)
- [Compatibility and upgrade notes](#compatibility-and-upgrade-notes)
- [Troubleshooting](#troubleshooting)

---

## Features

| Area | What you get |
|---|---|
| Endpoints | `Endpoint` for TCP or Unix sockets, `ParseEndpoint`, loopback/private-address policies, `Endpoint.Dialer`, `DefaultSocketPath` |
| Transport | Length-prefixed binary handshake messages; no `net/http` |
| Runtime state | `RuntimeStore` with atomic writes, fsync, portable file names, `List`/`Read`/`CleanupDead`/`CheckWritable` |
| Identity | Linux boot-ID + PID-namespace + start-tick identity; Darwin/Windows creation time; exact-match semantics |
| Discovery | PID-aware `Discover`, `UnreachableError`, service/version filters, custom `Accept` |
| Auto-start | `Manager` with start-lock serialization, repeated discovery, timeout, caller-owned `StartFunc` |
| Locks | Start lock, owner lock, listen lock; blocking and nonblocking acquisition |
| Security | Optional challenge/response HMAC proof bound to service, version, PID, network and address |
| Stop | Endpoint verification, caller shutdown request, exit proof, timeout |
| Process control | `ProcessAlive`, `ReadProcessIdentity`, identity comparison, `Kill` |
| Launch | `StartDetached` with platform-appropriate detachment and an `Exited` callback |

---

## Platform support

| Platform | Support | Notes |
|---|---|---|
| Linux | Full | Versioned process identity (`process_identity_v2`) |
| macOS (amd64, arm64) | Full | Creation-time identity from `kern.proc.pid` |
| Windows | Full | Protected-DACL private directories, `LockFileEx`, `DETACHED_PROCESS`, AF_UNIX sockets |
| FreeBSD, OpenBSD, NetBSD, DragonFly | Full primitives | Creation-time identity is reported as unknown |
| Solaris, AIX, Plan 9, js/wasm, wasip1 | Compiles, fail-closed | Private runtime directories, file locks, `Kill`, and identity are unavailable and return errors/unknown |

Runtime records, private directories, and Unix sockets are designed for
**current-user-local** daemons. No public bind behavior is enabled by default;
use `RequireLoopback` or `RequireNonPublic` when parsing TCP addresses.

---

## Requirements and installation

- Go **1.27.3** or newer (the module's `go` directive).
- No runtime dependencies. Tests use `github.com/stretchr/testify`.

```bash
go get github.com/malivvan/daemon
```

```go
import "github.com/malivvan/daemon"
```

---

## Quick start: a complete daemon and client

This program builds a daemon (`widgetd`), auto-starts it from the client side,
speaks a tiny newline protocol over the authenticated connection, and stops it.
It works on Linux, macOS, and Windows (AF_UNIX).

```go
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/malivvan/daemon"
)

const (
	service = "widgetd"
	version = "0.1.0"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serve()
		return
	}
	status()
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func runtimeStore() daemon.RuntimeStore {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return daemon.RuntimeStore{
		Dir:    filepath.Join(base, service, "runtime"),
		Prefix: service,
	}
}

func endpoint() daemon.Endpoint {
	path := daemon.DefaultSocketPath(service)
	if path == "" {
		fatal(errors.New("no private Unix socket path available"))
	}
	return daemon.Endpoint{Network: daemon.NetworkUnix, Address: path}
}

func serve() {
	store := runtimeStore()
	if err := store.CheckWritable(); err != nil {
		fatal(err)
	}
	owner, err := store.AcquireOwnerLock(context.Background())
	if err != nil {
		fatal(err)
	}
	defer owner()

	ep := endpoint()
	listener, err := daemon.Listen(context.Background(), ep, daemon.WithRuntimeStore(store))
	if err != nil {
		fatal(err)
	}

	rec := daemon.NewRuntimeRecord(service, version, ep)
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{Record: rec})

	path, err := store.Write(rec)
	if err != nil {
		fatal(err)
	}
	defer os.Remove(path)

	var stopOnce sync.Once
	stopped := make(chan struct{})
	shutdown := func() {
		stopOnce.Do(func() {
			close(stopped)
			_ = listener.Close()
		})
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if err := ping(context.Background(), conn); err != nil {
					return
				}
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				switch strings.TrimSpace(line) {
				case "widgets":
					_, _ = io.WriteString(conn, `{"widgets":["sprocket","cog"]}`+"\n")
				case "shutdown":
					_, _ = io.WriteString(conn, "ok\n")
					shutdown()
				default:
					_, _ = io.WriteString(conn, "unknown command\n")
				}
			}()
		}
	}()

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignals()
	select {
	case <-ctx.Done():
	case <-stopped:
	}
	shutdown()
}

func status() {
	store := runtimeStore()
	manager := daemon.Manager{
		Store: store,
		Discover: daemon.DiscoverOptions{
			Probe:           daemon.ProbeOptions{ExpectedService: service, Timeout: time.Second},
			RequirePIDAlive: true,
		},
		Compatible: func(_ daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return info.Version == version
		},
		Start: func(ctx context.Context) error {
			return daemon.StartDetached(ctx, daemon.StartDetachedOptions{Args: []string{"serve"}})
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec, info, err := manager.Ensure(ctx, 5*time.Second)
	if err != nil {
		var unreachable *daemon.UnreachableError
		if errors.As(err, &unreachable) {
			fmt.Fprintf(os.Stderr, "candidate pid %d at %s failed: %v\n",
				unreachable.Record.PID, unreachable.Endpoint.ConfigAddress(), unreachable.Err)
		}
		fatal(err)
	}
	fmt.Printf("daemon pid=%d version=%s at %s\n", info.PID, info.Version, rec.Endpoint().ConfigAddress())

	conn, err := rec.Endpoint().Dialer(daemon.DialerOptions{Timeout: time.Second}).DialContext(ctx)
	if err != nil {
		fatal(err)
	}
	if _, err := io.WriteString(conn, "widgets\n"); err != nil {
		_ = conn.Close()
		fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	_ = conn.Close()
	if err != nil {
		fatal(err)
	}
	fmt.Println("widgets:", strings.TrimSpace(line))

	err = daemon.Stop(ctx, rec, daemon.StopOptions{
		Probe: daemon.ProbeOptions{ExpectedService: service},
		Request: func(ctx context.Context, rec daemon.RuntimeRecord) error {
			conn, err := rec.Endpoint().Dialer(daemon.DialerOptions{Timeout: time.Second}).DialContext(ctx)
			if err != nil {
				return nil // already gone
			}
			defer conn.Close()
			if _, err := io.WriteString(conn, "shutdown\n"); err != nil {
				return nil
			}
			_, err = bufio.NewReader(conn).ReadString('\n')
			return err
		},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		fatal(err)
	}
	fmt.Println("daemon stopped")
}
```

Build and run:

```bash
go mod init example.com/widgetd
go get github.com/malivvan/daemon
go build -o widgetd .
./widgetd          # discovers or auto-starts, queries, then stops the daemon
./widgetd serve    # run the daemon in the foreground
```

What happens:

1. `serve` acquires the **owner lock**, prepares a private socket directory,
   binds with the **listen lock**, writes an atomic runtime record, and starts
   accepting connections.
2. Every accepted connection first runs the **daemon handshake**
   (`ping(context.Background(), conn)`). Only after it succeeds does the
   application read its own protocol (`widgets` / `shutdown`).
3. `status` calls `Manager.Ensure`, which discovers existing records under the
   **start lock**, probes them with the same handshake, and launches
   `./widgetd serve` through `StartDetached` when none is compatible.
4. `Endpoint.Dialer` performs the handshake automatically, so the client can
   write application data as soon as `DialContext` returns.
5. `Stop` verifies the endpoint, sends the shutdown request, and waits for the
   listener to disappear.
6. The runtime file is removed by the daemon on exit.

---

## Core concepts

### Endpoints and dialers

An `Endpoint` is either:

- `daemon.Endpoint{Network: daemon.NetworkTCP, Address: "127.0.0.1:7373"}`, or
- `daemon.Endpoint{Network: daemon.NetworkUnix, Address: "/run/user/1000/svc/daemon.sock"}`.

`RuntimeRecord.Address` may store a Unix socket as `unix:///path`;
`RuntimeRecord.Endpoint()` normalizes it back to `NetworkUnix`.

Key helpers:

- `Endpoint.IsUnix()` — true for Unix sockets.
- `Endpoint.ConfigAddress()` — `unix:///path` or `host:port`, stable for config.
- `Endpoint.Port()` — TCP port, or `0`.
- `Endpoint.BaseURL()` — `http://localhost` or `http://host:port`, for callers
  that layer HTTP on top.
- `Endpoint.Listen()` / `Endpoint.ListenContext(ctx)` — raw bind without the
  Unix stale-socket protections (prefer the package-level `Listen`).
- `Endpoint.Dialer(opts)` — a `*Dialer` that connects **and performs the daemon
  handshake** before returning the connection.

```go
type DialerOptions struct {
	Timeout   time.Duration // bounds connecting and the handshake
	KeepAlive time.Duration // TCP keep-alive
	Proof     *Proof        // optional: authenticate the handshake
}
```

```go
conn, err := ep.Dialer(daemon.DialerOptions{Timeout: time.Second}).DialContext(ctx)
if err != nil {
	return err
}
defer conn.Close()
// The handshake is complete; speak the application protocol.
```

The returned connection has no deadline and is ready for application data. If
`Proof` is `nil`, the handshake is a plain ping; if it is set, the daemon must
prove possession of the same key before `DialContext` succeeds.

Address policies for `ParseEndpoint`:

- `daemon.RequireLoopback` — only literal loopback IPs or `localhost`.
- `daemon.RequireNonPublic` — loopback, RFC 1918 private, link-local, and CGNAT
  (`100.64.0.0/10`) addresses; rejects public and unspecified (`0.0.0.0`, `::`).

### The daemon handshake

Every daemon connection starts with one framed message exchange:

1. The client sends a `PingRequest` (optionally carrying a 32-byte challenge).
2. The daemon answers with a `PingResponse` (identity, optional proof, or an
   error).

The frame on the wire is a **4-byte big-endian length** followed by the encoded
message payload. Frames are capped at **1 MiB**. `PingRequest` and
`PingResponse` have dependency-free `Encode`/`Decode` methods using only
`encoding/binary`; the package's dialer and handler add the length prefix.

```go
type PingRequest struct {
	Challenge []byte // empty for an unproven ping
}

func (m PingRequest) Encode() ([]byte, error)
func (m *PingRequest) Decode(data []byte) error

type PingResponse struct {
	Info  PingInfo
	Proof []byte
	Error string
}

func (m PingResponse) Encode() ([]byte, error)
func (m *PingResponse) Decode(data []byte) error

type PingInfo struct {
	OK      bool
	Service string
	Version string
	PID     int
}
```

Payload layouts:

| Message | Layout |
|---|---|
| `PingRequest` | type `1`, protocol version `1`, `uint16` challenge length, challenge |
| `PingResponse` | type `2`, protocol version `1`, status flags, service, version, `int64` PID, proof, error |

Strings and byte fields are length-prefixed with `uint16`. `Decode` rejects
wrong message types, unsupported versions, truncation, trailing bytes, and
oversized fields.

Server side: `NewPingHandler` returns a `PingHandler`, a function that handles
**one accepted connection**:

```go
type PingHandler func(ctx context.Context, conn net.Conn) error

func NewPingHandler(opts PingHandlerOptions) PingHandler

type PingHandlerOptions struct {
	Record RuntimeRecord // identity announced and bound by proof
	Proof  *Proof        // optional: answer proof challenges
}
```

The handler reads exactly one request, writes one response, and returns. It
does **not** close the connection; the caller continues with its own protocol.
This is why the application must run the handler before reading app data.

`Proof.NewPingHandler(rec)` is a convenience that validates the record's
identity up front.

### Runtime records

A daemon publishes a JSON record at
`<runtime-dir>/<prefix>.<pid>.json` (mode `0644`, written atomically and fsynced):

```json
{
  "pid": 12345,
  "process_identity_v2": "linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:4026531836:1234567",
  "network": "unix",
  "address": "/run/user/1000/widgetd/daemon.sock",
  "service": "widgetd",
  "version": "0.1.0",
  "started_at": "2026-10-25T18:00:00.123456789Z",
  "metadata": {
    "db_path": "/home/me/.local/share/widgetd/db"
  }
}
```

On macOS and Windows the identity lives in the legacy field instead:

```json
{
  "pid": 12345,
  "process_identity": "1761411600123",
  "network": "tcp",
  "address": "127.0.0.1:7373",
  "service": "widgetd"
}
```

Rules enforced by `RuntimeStore`:

- `Dir` must be absolute and become a **private current-user directory**
  (mode `0700` on Unix; a protected DACL on Windows). It is created/repaired.
- `Prefix` defaults to `"daemon"`. It must be a portable single file name
  (no separators, reserved Windows device names, trailing dots/spaces, invalid
  UTF-8, or forbidden characters) and at most **214 bytes**.
- Records are written with a staging file named `.<name>.tmp-*`; those staging
  names and the write-check probe never match the record namespace, so an
  interrupted write is never read or deleted as a record.
- `List` ignores malformed files, PID-mismatched filenames, non-records, and
  staging leftovers; it sorts by `StartedAt`, then PID.
- `Read` refuses symlinks, non-regular files, and files not owned by the
  current user.
- `CleanupDead` removes only records whose filename PID matches the record and
  whose process is **proven gone**. Records this caller cannot judge are kept.
- `Write` fills a zero `StartedAt` with the current UTC time and returns the
  final path. If post-publication durability failed
  (`errPublished`/`errNotDurable`, unexported), it still returns that path with
  the error, because the record is already visible.

### Process identity and liveness

`ProcessIdentity` is an opaque, exact-match value:

| Platform | Field | Encoding |
|---|---|---|
| Linux | `ProcessIdentityV2` | `linux-v1:<boot-id>:<pid-namespace-inode>:<start-ticks>` |
| macOS | `ProcessIdentity` | Unix milliseconds of process creation time |
| Windows | `ProcessIdentity` | Unix milliseconds of process creation time |
| Other builds | — | Unknown |

Use these helpers instead of comparing PIDs by hand:

- `ReadProcessIdentity(pid)` — current identity, or `ok == false`.
- `CompareProcessIdentity(pid, recorded)` — `Match`, `Mismatch`, or `Unknown`.
- `CompareRuntimeProcessIdentity(rec)` — uses the strongest identity in `rec`.
- `RuntimeProcessScope(rec)` — what the identity alone proves:

  | Scope | Meaning | Safe action |
  |---|---|---|
  | `ProcessScopeLocal` | The PID names a process this caller can check | PID check applies |
  | `ProcessScopeForeign` | Another PID namespace on this boot | Keep the record; never signal/delete by PID |
  | `ProcessScopeUnknown` | Identity/boot ID/namespace unreadable | Keep the record; never signal/delete by PID |
  | `ProcessScopeGone` | Identity proves the process gone | Record may be cleaned up |

- `RuntimeProcessAlive(rec)` — `false` only when the record is local and the PID
  is gone, or the identity proves it gone; `true` for foreign/unknown records so
  they are not discarded.
- `RuntimeRecordBlocksStart(rec)` — true for a live `Local`/`Unknown` PID whose
  identity is not a definite mismatch, never for the caller's own PID.
- `ProcessAlive(pid)` — plain PID liveness (Unix signal 0; Windows exit status
  `STILL_ACTIVE`).
- `Kill(pid)` — immediate termination (`SIGKILL`/`TerminateProcess`). It does
  **not** verify identity; check `CompareRuntimeProcessIdentity` first.

A Linux record whose boot ID differs from the caller's own is judged **gone**,
even if that PID is alive locally. A record from a foreign PID namespace is kept
unless the caller is in the initial PID namespace and can prove no nested
process has the recorded PID and start ticks.

### Locks

Every `RuntimeStore` exposes three kinds of locks, all keyed by absolute files
inside the private runtime directory:

| Lock | Path | Purpose |
|---|---|---|
| Start | `<prefix>.lock` | Serialize discovery, replacement, and launch decisions |
| Owner | `<prefix>.lock.owner` | Held for a daemon's full writable lifetime |
| Listen | `<prefix>.listen.lock` | Serialize Unix-socket probe/remove/bind |

```go
release, err := store.AcquireStartLock(ctx)      // blocks until free
defer release()

probe, acquired, err := store.TryAcquireStartLock(ctx) // nonblocking
if err != nil {
    // lock state unknown: do not launch or clean up
}
if acquired {
    defer probe()
}

owner, err := store.AcquireOwnerLock(ctx)        // lifetime ownership
defer owner()
```

Semantics that matter:

- Contention is **authoritative**. A failed nonblocking acquisition
  (`acquired == false`, `err == nil`) means someone else is starting a daemon.
- An **error** means the state is unknown; do not infer a launch decision.
- Local (same-process) contention is included.
- Lock files are **never removed**. Keeping the same filesystem object matters;
  the OS releases the lock when the holder exits.
- Cleanup of caller-owned startup state must finish while holding the start
  lock. A released probe or lock grants no reservation for a later launch.
- `Manager.Ensure` acquires the start lock for you and re-runs discovery under
  it.

Unix-socket `Listen` uses the listen lock. If you pass
`daemon.WithRuntimeStore(store)`, it uses `<prefix>.listen.lock`; with
`WithListenLockPath(path)` you choose the file; otherwise it uses
`<socket-path>.lock`.

### Proof of possession

`Proof` is **optional**. It is used only when a `Proof` is supplied:

- `DialerOptions.Proof` authenticates the connection during `DialContext`.
- `DiscoverOptions.Proof` requires proof for every probed record.
- `StopOptions.Proof` requires proof before the shutdown request.
- `PingHandlerOptions.Proof` lets the daemon answer challenges; without it, a
  challenged handshake is refused.

The protocol:

1. The client generates a 32-byte nonce and sends it as the `PingRequest`
   challenge.
2. The daemon computes an HMAC-SHA-256 over a domain separator, the nonce, and
   the daemon identity: service, version, PID, network, and address.
3. The client recomputes the HMAC for the endpoint it dialed and the identity in
   the response, and rejects a missing or mismatched proof.

The shared token is never transmitted. `Proof` redacts its key in `%v`, `%+v`,
and `%#v` formatting. A proof only establishes possession of the key and the
bound identity; it is not an application authorization token.

**Ordering rule:** do not send an application credential until the handshake has
succeeded for that endpoint. A custom `FindFunc` must preserve this ordering.

### Stopping a daemon

`Stop` is a client-side, verified shutdown sequence:

1. Requires a positive PID and a `Request` function.
2. Without `Exited`, a refused connection confirms exit before any request.
3. Probes the endpoint with the handshake, using `Proof` only if provided.
4. Calls `Request` only after verification.
5. Waits for exit. With `Exited`, only a `true` result confirms exit; without
   it, connection refusal or a different positive PID ends the wait.
6. Sends **no signals** and removes **no files**.

`StopOptions.Timeout` defaults to 30 seconds. A connection dropped during the
shutdown request should be treated as success by returning `nil` from
`Request`; `Stop` then waits for actual exit.

---

## Usage guide

### Parsing endpoints and dialing

```go
func parseEndpoints() {
	ep, err := daemon.ParseEndpoint("unix:///run/user/1000/tool/daemon.sock", daemon.ParseEndpointOptions{})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(ep.Network, ep.Address, ep.ConfigAddress())

	ep, err = daemon.ParseEndpoint("", daemon.ParseEndpointOptions{
		DefaultTCPAddress: "127.0.0.1:7373",
		TCPPolicy:         daemon.RequireLoopback,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(ep.ConfigAddress(), ep.Port())
}

func dialEndpoint(ep daemon.Endpoint) (net.Conn, error) {
	return ep.Dialer(daemon.DialerOptions{
		Timeout:   time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext(context.Background())
}
```

`DefaultSocketPath(service)` returns a short per-user socket path:

- `$XDG_RUNTIME_DIR/<service>/daemon.sock` when that directory can be made
  private and the path fits;
- otherwise `<os.TempDir()>/<service>-<user-id>/daemon.sock`;
- otherwise `""`.

Unix socket path limits are enforced by `ParseEndpoint`:
**104 bytes** on macOS/BSD and **108 bytes** on Linux/Windows
(`daemon.MaxUnixPathLen`).

### Serving a daemon

The quick-start `serve` function is the recommended shape:

1. `store.CheckWritable()` as an advisory preflight.
2. `store.AcquireOwnerLock(ctx)` for the writable lifetime.
3. `daemon.Listen(ctx, ep, daemon.WithRuntimeStore(store))` to serialize Unix
   stale-socket cleanup and bind.
4. Build a `PingHandler` with `daemon.NewPingHandler` (or
   `Proof.NewPingHandler` when proof is configured).
5. `store.Write(daemon.NewRuntimeRecord(service, version, ep))` **after** the
   listener is bound.
6. Accept connections, run the ping handler, then run the application protocol.
7. Close the listener to stop accepting; remove the runtime file on exit.

```go
ping := daemon.NewPingHandler(daemon.PingHandlerOptions{Record: rec})

for {
	conn, err := listener.Accept()
	if err != nil {
		return
	}
	go func() {
		defer conn.Close()
		if err := ping(ctx, conn); err != nil {
			return
		}
		// The connection is authenticated (when proof is configured).
		// Read and write the application protocol here.
	}()
}
```

`daemon.Listen` for Unix sockets:

- requires an absolute socket path inside a private directory;
- refuses to remove a non-socket path;
- probes an existing socket; a live listener is an error;
- removes only a stale socket, under the listen lock;
- never touches a path it cannot identify as the stale socket.

For TCP endpoints, `Listen` delegates to the normal network listener and the
listen lock is not used.

### Probing and discovery

```go
func discover(ctx context.Context, store daemon.RuntimeStore) {
	rec, info, ok, err := daemon.Discover(ctx, store, daemon.DiscoverOptions{
		Probe: daemon.ProbeOptions{
			ExpectedService: "tool",
			Timeout:         time.Second,
		},
		RequirePIDAlive: true,
		Accept: func(rec daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return info.Version == "1.2.3"
		},
	})
	if err != nil {
		var unreachable *daemon.UnreachableError
		if errors.As(err, &unreachable) {
			fmt.Printf("pid %d at %s is live but unreachable: %v\n",
				unreachable.Record.PID, unreachable.Endpoint.ConfigAddress(), unreachable.Err)
		}
		return
	}
	if !ok {
		fmt.Println("no daemon is running")
		return
	}
	fmt.Println(rec.PID, info.Service, info.Version)
}
```

`daemon.Probe(ctx, ep, opts)` performs a plain handshake and returns `PingInfo`.
`(*Proof).Probe(ctx, rec, opts)` performs a proven handshake and also verifies
that the response identity matches `rec`.

Important details:

- `RequirePIDAlive: true` skips dead records and definite identity mismatches.
  A live record whose probe fails is returned as an `UnreachableError` when no
  later record succeeds. Without `RequirePIDAlive`, a failed probe is simply
  skipped and `ok == false` means "nothing reachable".
- `Accept` filters compatible records; `Manager.Compatible` is applied on top.
- `Probe.ExpectedService` rejects a daemon with the wrong service string.
- `DiscoverOptions.Proof` enables proof-of-possession probing.

### Auto-starting with Manager.Ensure

`Manager` ties discovery, start locking, and launching together:

```go
manager := daemon.Manager{
	Store: store,
	Discover: daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "tool", Timeout: time.Second},
		RequirePIDAlive: true,
	},
	Compatible: func(_ daemon.RuntimeRecord, info daemon.PingInfo) bool {
		return info.Version == "1.2.3"
	},
	Start: func(ctx context.Context) error {
		return daemon.StartDetached(ctx, daemon.StartDetachedOptions{
			Args: []string{"serve"},
		})
	},
}

rec, info, err := manager.Ensure(ctx, 5*time.Second)
```

Behavior:

- `Find` uses `FindFunc` when set; otherwise `Discover` with the
  `Compatible` predicate folded into `Accept`.
- `Ensure` runs discovery, acquires `<prefix>.lock`, **re-runs discovery under
  the lock**, calls `Start`, then polls `Find` every 50 ms until the deadline.
- `timeout == 0` means 5 seconds. Use a positive timeout.
- An `UnreachableError` from discovery prevents a second daemon from starting;
  it is returned to the caller.
- `Start` is caller-owned. The package never invents an application launch
  command.

### Authenticated daemons with Proof

Complete server and client (the same shape as the quick start, with proof):

```go
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/malivvan/daemon"
)

const (
	service = "secretd"
	version = "1.0.0"
)

func main() {
	token := []byte(os.Getenv("SECRETD_TOKEN"))
	if len(token) == 0 {
		panic("SECRETD_TOKEN is required")
	}
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		serve(token)
		return
	}
	call(token)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}

func runtimeStore() daemon.RuntimeStore {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return daemon.RuntimeStore{Dir: filepath.Join(base, service, "runtime"), Prefix: service}
}

func endpoint() daemon.Endpoint {
	path := daemon.DefaultSocketPath(service)
	if path == "" {
		fatal(errors.New("no private Unix socket path available"))
	}
	return daemon.Endpoint{Network: daemon.NetworkUnix, Address: path}
}

func serve(token []byte) {
	store := runtimeStore()
	proof, err := daemon.NewProof(token)
	if err != nil {
		fatal(err)
	}
	ep := endpoint()
	listener, err := daemon.Listen(context.Background(), ep, daemon.WithRuntimeStore(store))
	if err != nil {
		fatal(err)
	}

	rec := daemon.NewRuntimeRecord(service, version, ep)
	ping, err := proof.NewPingHandler(rec)
	if err != nil {
		fatal(err)
	}
	path, err := store.Write(rec)
	if err != nil {
		fatal(err)
	}
	defer os.Remove(path)

	var stopOnce sync.Once
	stopped := make(chan struct{})
	shutdown := func() {
		stopOnce.Do(func() {
			close(stopped)
			_ = listener.Close()
		})
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				if err := ping(context.Background(), conn); err != nil {
					return
				}
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil {
					return
				}
				switch strings.TrimSpace(line) {
				case "secret":
					_, _ = io.WriteString(conn, "classified\n")
				case "shutdown":
					_, _ = io.WriteString(conn, "ok\n")
					shutdown()
				default:
					_, _ = io.WriteString(conn, "unknown command\n")
				}
			}()
		}
	}()

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stopSignals()
	select {
	case <-ctx.Done():
	case <-stopped:
	}
	shutdown()
}

func call(token []byte) {
	store := runtimeStore()
	proof, err := daemon.NewProof(token)
	if err != nil {
		fatal(err)
	}
	manager := daemon.Manager{
		Store: store,
		Discover: daemon.DiscoverOptions{
			Probe:           daemon.ProbeOptions{ExpectedService: service, Timeout: time.Second},
			RequirePIDAlive: true,
			Proof:           proof,
		},
		Compatible: func(_ daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return info.Version == version
		},
		Start: func(ctx context.Context) error {
			return daemon.StartDetached(ctx, daemon.StartDetachedOptions{Args: []string{"serve"}})
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec, _, err := manager.Ensure(ctx, 5*time.Second)
	if err != nil {
		var unreachable *daemon.UnreachableError
		if errors.As(err, &unreachable) {
			fmt.Fprintf(os.Stderr, "pid %d at %s failed proof: %v\n",
				unreachable.Record.PID, unreachable.Endpoint.ConfigAddress(), unreachable.Err)
		}
		fatal(err)
	}

	// Discovery proved the endpoint; the connection is authenticated by the
	// same handshake before any application data is exchanged.
	conn, err := rec.Endpoint().Dialer(daemon.DialerOptions{
		Timeout: time.Second,
		Proof:   proof,
	}).DialContext(ctx)
	if err != nil {
		fatal(err)
	}
	if _, err := io.WriteString(conn, "secret\n"); err != nil {
		_ = conn.Close()
		fatal(err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	_ = conn.Close()
	if err != nil {
		fatal(err)
	}
	fmt.Println("response:", strings.TrimSpace(line))

	err = daemon.Stop(ctx, rec, daemon.StopOptions{
		Probe: daemon.ProbeOptions{ExpectedService: service},
		Proof: proof,
		Request: func(ctx context.Context, rec daemon.RuntimeRecord) error {
			conn, err := rec.Endpoint().Dialer(daemon.DialerOptions{
				Timeout: time.Second,
				Proof:   proof,
			}).DialContext(ctx)
			if err != nil {
				return nil // already gone
			}
			defer conn.Close()
			if _, err := io.WriteString(conn, "shutdown\n"); err != nil {
				return nil
			}
			_, err = bufio.NewReader(conn).ReadString('\n')
			return err
		},
		Timeout: 10 * time.Second,
	})
	if err != nil {
		fatal(err)
	}
	fmt.Println("daemon stopped")
}
```

Invoke with `SECRETD_TOKEN=... ./secretd`. The daemon refuses challenged
handshakes from clients that cannot prove the same token; an unproven client can
still perform a plain ping unless the application chooses to require proof.

If the endpoint is reachable by other users, keep application secrets out of
`RuntimeRecord.Metadata`; records are mode `0644` (the directory is private, but
metadata is not a secret store).

### Stopping a daemon

`Proof` is optional. To wait for the actual process to exit (not just for the
listener to close), supply `Exited`:

```go
func stopWithExited(ctx context.Context, rec daemon.RuntimeRecord) {
	exited := func(ctx context.Context, rec daemon.RuntimeRecord) (bool, error) {
		// Use application-specific evidence, such as an ownership lock that
		// can be acquired only after the daemon has fully exited.
		return !daemon.ProcessAlive(rec.PID), nil
	}
	err := daemon.Stop(ctx, rec, daemon.StopOptions{
		Probe:   daemon.ProbeOptions{ExpectedService: "tool"},
		Exited:  exited,
		Timeout: 30 * time.Second,
		Request: func(ctx context.Context, rec daemon.RuntimeRecord) error {
			conn, err := rec.Endpoint().Dialer(daemon.DialerOptions{Timeout: time.Second}).DialContext(ctx)
			if err != nil {
				return nil // the daemon dropped the connection while shutting down
			}
			defer conn.Close()
			_, err = io.WriteString(conn, "shutdown\n")
			return err
		},
	})
	if err != nil {
		fmt.Println(err)
	}
}
```

Without `Exited`, `Stop` ends when the endpoint refuses connections or a
replacement answers with a different PID. That proves the listener is gone, not
that the process exited. Use `Exited` when exit matters.

### Runtime-store housekeeping

```go
func housekeeping(store daemon.RuntimeStore) {
	if err := store.CheckWritable(); err != nil {
		log.Fatal(err)
	}
	records, err := store.List()
	if err != nil {
		log.Fatal(err)
	}
	for _, rec := range records {
		fmt.Println(rec.PID, rec.Endpoint().ConfigAddress(), daemon.RuntimeProcessAlive(rec))
	}
	removed, err := store.CleanupDead()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("removed", removed, "dead records")
}
```

`CheckWritable` creates and removes a probe file, so a successful call leaves
nothing behind. It is advisory: it does not guarantee that a child process has
the same permissions.

### Locks in practice

```go
func locks(ctx context.Context, store daemon.RuntimeStore) {
	release, err := store.AcquireStartLock(ctx)
	if err != nil {
		log.Fatal(err)
	}
	// Decide whether to start a daemon here, then release.
	release()

	probeRelease, acquired, err := store.TryAcquireStartLock(ctx)
	switch {
	case err != nil:
		fmt.Println("lock state unknown:", err)
	case acquired:
		// Another goroutine or process is not starting a daemon right now.
		probeRelease()
	default:
		fmt.Println("another starter holds the lock")
	}

	ownerRelease, err := store.AcquireOwnerLock(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer ownerRelease()
}
```

Use the start lock for **short** discovery/launch decisions. Hold the owner lock
for the daemon's whole writable lifetime. Use `TryAcquireStartLock` for probes,
and remember its result is advisory once released.

### Process controls and identity

```go
func processControls() {
	pid := os.Getpid()
	fmt.Println("alive:", daemon.ProcessAlive(pid))

	identity, ok := daemon.ReadProcessIdentity(pid)
	if !ok {
		fmt.Println("identity unavailable on this platform")
		return
	}
	if daemon.CompareProcessIdentity(pid, identity) != daemon.ProcessIdentityMatch {
		fmt.Println("identity does not match")
		return
	}
	rec := daemon.NewRuntimeRecord("tool", "1.2.3", daemon.Endpoint{Network: daemon.NetworkTCP, Address: "127.0.0.1:7373"})
	fmt.Println("scope:", daemon.RuntimeProcessScope(rec))
	fmt.Println("record live:", daemon.RuntimeProcessAlive(rec))
	fmt.Println("blocks start:", daemon.RuntimeRecordBlocksStart(rec))
}
```

Force-terminate only a process whose identity you have proven:

```go
if daemon.CompareRuntimeProcessIdentity(rec) == daemon.ProcessIdentityMatch {
	if err := daemon.Kill(rec.PID); err != nil {
		log.Println("kill failed:", err)
	}
}
```

`Kill` itself only checks that the PID is positive. It never accepts an unknown
identity as permission to signal.

### Starting a detached child

```go
func detachExample() {
	logFile, err := os.OpenFile("/tmp/tool.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Println(err)
		return
	}
	defer logFile.Close()
	exited := make(chan error, 1)
	err = daemon.StartDetached(context.Background(), daemon.StartDetachedOptions{
		Executable:      "/usr/local/bin/tool",
		Args:            []string{"serve"},
		Dir:             "/",
		Env:             os.Environ(),
		Stdout:          logFile,
		Stderr:          logFile,
		RefuseEphemeral: true,
		AfterStart: func(cmd *exec.Cmd) {
			log.Printf("started pid %d", cmd.Process.Pid)
		},
		Exited: func(err error) {
			exited <- err
		},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	select {
	case err := <-exited:
		fmt.Println("child exited:", err)
	case <-time.After(time.Second):
	}
}
```

Behavior:

- `Executable` defaults to `os.Executable()`.
- `Env == nil` inherits the caller's environment; a non-nil slice is used as-is.
- Unix uses `Setsid`; Windows uses `CREATE_NEW_PROCESS_GROUP |
  DETACHED_PROCESS` (never `CREATE_NO_WINDOW`, which leaves `CONIN$` available
  and can block terminal-probing libraries).
- **`StartDetached` reaps the child itself.** Do not call `cmd.Wait()`. Use the
  `Exited` callback to learn the child's exit result.
- `RefuseEphemeral` rejects `*.test` and go-build binaries. Enable it in
  production; it also prevents `go run .` demos from auto-starting.

---

## API reference

### Functions

| Function | Description |
|---|---|
| `ParseEndpoint(raw string, opts ParseEndpointOptions) (Endpoint, error)` | Parse `host:port`, `http://host:port`, or `unix:///path`. Empty input falls back to `DefaultUnixPath`, then `DefaultTCPAddress`. |
| `RequireLoopback(addr string) error` | Reject non-loopback TCP hosts. |
| `RequireNonPublic(addr string) error` | Reject public and unspecified TCP hosts. |
| `DefaultSocketPath(service string) string` | Per-user private Unix socket path, or `""`. |
| `Listen(ctx, ep Endpoint, options ...ListenOption) (net.Listener, error)` | Bind with Unix stale-socket protection. |
| `NewPingHandler(opts PingHandlerOptions) PingHandler` | Handshake responder for one accepted connection. |
| `Probe(ctx, ep Endpoint, opts ProbeOptions) (PingInfo, error)` | Plain handshake probe. |
| `Discover(ctx, store RuntimeStore, opts DiscoverOptions) (RuntimeRecord, PingInfo, bool, error)` | Runtime-file discovery. |
| `StartDetached(ctx, opts StartDetachedOptions) error` | Start a detached child and reap it. |
| `IsEphemeralExecutable(exe string) bool` | Detect test/go-build binaries. |
| `Stop(ctx, rec RuntimeRecord, opts StopOptions) error` | Verified shutdown request and exit wait. |
| `ProcessAlive(pid int) bool` | Platform PID liveness. |
| `ReadProcessIdentity(pid int) (ProcessIdentity, bool)` | Exact process-creation identity. |
| `CompareProcessIdentity(pid int, recorded ProcessIdentity) ProcessIdentityStatus` | Compare identity with a live PID. |
| `CompareRuntimeProcessIdentity(rec RuntimeRecord) ProcessIdentityStatus` | Compare the strongest identity in a record. |
| `RuntimeProcessScope(rec RuntimeRecord) ProcessScope` | Classify what the identity proves. |
| `RuntimeProcessAlive(rec RuntimeRecord) bool` | Identity-aware record liveness. |
| `RuntimeRecordBlocksStart(rec RuntimeRecord) bool` | Whether a record should block a new start. |
| `Kill(pid int) error` | Immediate termination; no identity check. |
| `NewProof(key []byte) (*Proof, error)` | Build a proof capability from a shared token. |

### `Endpoint`

```go
type Endpoint struct {
	Network string `json:"network"`
	Address string `json:"address"`
}
```

| Method | Description |
|---|---|
| `Dialer(DialerOptions) *Dialer` | Dial and perform the daemon handshake. |
| `IsUnix()` | Network is `unix`. |
| `ConfigAddress()` | `unix:///path` or `host:port`. |
| `BaseURL()` | `http://localhost` or `http://host:port`, for HTTP layering. |
| `Port()` | TCP port, otherwise `0`. |
| `Listen()` / `ListenContext(ctx)` | Raw listener bind (prefer `daemon.Listen`). |

### `Dialer` and `DialerOptions`

```go
type DialerOptions struct {
	Timeout   time.Duration
	KeepAlive time.Duration
	Proof     *Proof
}
```

| Field | Description |
|---|---|
| `Timeout` | Bounds connecting and the handshake. Zero relies on the context. |
| `KeepAlive` | TCP keep-alive period passed to `net.Dialer`. |
| `Proof` | Optional proof of possession during the handshake. |

| Method | Description |
|---|---|
| `(*Dialer).DialContext(ctx) (net.Conn, error)` | Connect, handshake, and return a connection ready for the application protocol. |
| `(*Dialer).Endpoint() Endpoint` | The endpoint this dialer targets. |

### Handshake messages

| Type | Fields | Methods |
|---|---|---|
| `PingRequest` | `Challenge []byte` | `Encode() ([]byte, error)`, `Decode([]byte) error` |
| `PingResponse` | `Info PingInfo`, `Proof []byte`, `Error string` | `Encode() ([]byte, error)`, `Decode([]byte) error` |
| `PingInfo` | `OK bool`, `Service string`, `Version string`, `PID int` | — |
| `PingHandlerOptions` | `Record RuntimeRecord`, `Proof *Proof` | — |
| `PingHandler` | `func(ctx context.Context, conn net.Conn) error` | — |

`Encode` writes the payload only. The package frames it with a 4-byte
big-endian length when using `net.Conn`. Frames are limited to 1 MiB.

### `RuntimeRecord`

| Field | Description |
|---|---|
| `PID` | Recorded process ID. |
| `ProcessIdentity` | Legacy identity: Unix milliseconds on macOS/Windows. |
| `ProcessIdentityV2` | Versioned identity: `linux-v1:...` on Linux. |
| `Network`, `Address` | Endpoint (`address` may be `unix:///path`). |
| `Service`, `Version` | Application identity checked by probes. |
| `StartedAt` | UTC start time; filled by `Write` when zero. |
| `Metadata` | Optional application data. Do not store secrets. |
| `SourcePath` | Set by `Read`/`List`; ignored on disk. |

`NewRuntimeRecord(service, version, ep)` builds a record for the current process.
`RuntimeRecord.Endpoint()` returns the normalized endpoint.

### `RuntimeStore`

```go
type RuntimeStore struct {
	Dir    string
	Prefix string
}
```

| Method | Description |
|---|---|
| `CheckWritable() error` | Advisory create/remove probe in `Dir`. |
| `Path(pid int) (string, error)` | `<Dir>/<prefix>.<pid>.json`. |
| `LockPath() (string, error)` | `<Dir>/<prefix>.lock`. |
| `ListenLockPath() (string, error)` | `<Dir>/<prefix>.listen.lock`. |
| `Write(rec RuntimeRecord) (string, error)` | Atomic fsynced write; returns the final path. |
| `Read(path string) (RuntimeRecord, error)` | Secure read of one record. |
| `List() ([]RuntimeRecord, error)` | Valid records, sorted. |
| `CleanupDead() (int, error)` | Remove records proven dead; returns the count. |
| `AcquireStartLock(ctx) (func(), error)` | Blocking start lock. |
| `TryAcquireStartLock(ctx) (release func(), acquired bool, err error)` | Nonblocking start lock. |
| `AcquireOwnerLock(ctx) (func(), error)` | Lifetime owner lock. |

### `Manager`

| Field | Description |
|---|---|
| `Store` | Runtime store; always used for the start lock. |
| `Discover` | Options for built-in discovery. |
| `Compatible` | Extra compatibility predicate. |
| `FindFunc` | Custom discovery; must honor `ctx` and proof ordering. |
| `Start` | Caller-owned auto-start function. |

| Method | Description |
|---|---|
| `Find(ctx)` | Discover a live compatible daemon. |
| `Ensure(ctx, timeout)` | Find or start one, with start-lock serialization. |

### `ProbeOptions` / `DiscoverOptions`

| Type | Fields |
|---|---|
| `ProbeOptions` | `ExpectedService`, `Timeout` (default 1s) |
| `DiscoverOptions` | `Probe`, `Proof`, `RequirePIDAlive`, `Accept` |

`UnreachableError` carries `Record`, `Endpoint`, and `Err`, and unwraps to
`ErrDaemonUnreachable` plus the handshake error.

### `Proof`

| Method | Description |
|---|---|
| `NewProof(key)` | Copies the shared token. Empty keys are rejected. |
| `(*Proof).NewPingHandler(rec)` | Handshake handler that answers proof challenges. |
| `(*Proof).Probe(ctx, rec, opts)` | Probe and verify proof-of-possession plus the record identity. |
| `(Proof).Format` | Redacts the key in formatting. |

### `StopOptions`

| Field | Description |
|---|---|
| `Probe` | Liveness probe options. |
| `Proof` | Optional proof before the request. |
| `Request` | Shutdown request; called only after verification. |
| `Exited` | Optional exit proof; only `true` confirms exit. |
| `Timeout` | Whole-call bound; default 30s. |

### `StartDetachedOptions`

| Field | Description |
|---|---|
| `Executable` | Path; defaults to `os.Executable()`. |
| `Args`, `Dir`, `Env` | Passed to the child (`Env == nil` inherits). |
| `Stdout`, `Stderr` | Child output writers. |
| `RefuseEphemeral` | Reject test/go-build binaries. |
| `AfterStart(*exec.Cmd)` | Called after a successful start. |
| `Exited(error)` | Receives the reaped child's exit result. |

### Process types

```go
type ProcessIdentity string

type ProcessIdentityStatus uint8
const (
	ProcessIdentityUnknown ProcessIdentityStatus = iota
	ProcessIdentityMatch
	ProcessIdentityMismatch
)

type ProcessScope uint8
const (
	ProcessScopeUnknown ProcessScope = iota
	ProcessScopeLocal
	ProcessScopeForeign
	ProcessScopeGone
)
```

### Listen options

```go
daemon.Listen(ctx, ep,
	daemon.WithRuntimeStore(store),
	daemon.WithListenLockPath("/path/to/daemon.lock"),
	daemon.WithStaleSocketProbeTimeout(500*time.Millisecond),
)
```

`WithRuntimeStore` is ignored when `WithListenLockPath` is also supplied.

### Other option types

| Type | Fields |
|---|---|
| `ParseEndpointOptions` | `DefaultTCPAddress`, `DefaultUnixPath`, `TCPPolicy` |
| `PingHandlerOptions` | `Record`, `Proof` |
| `DiscoverOptions` | `Probe`, `Proof`, `RequirePIDAlive`, `Accept` |
| `ProbeOptions` | `ExpectedService`, `Timeout` (default 1s) |
| `StopOptions` | `Probe`, `Proof`, `Request`, `Exited`, `Timeout` (default 30s) |
| `StartDetachedOptions` | `Executable`, `Args`, `Dir`, `Env`, `Stdout`, `Stderr`, `RefuseEphemeral`, `AfterStart`, `Exited` |

`ListenOption` values are `WithRuntimeStore(store)`,
`WithListenLockPath(path)`, and `WithStaleSocketProbeTimeout(duration)`.

---

## Error reference

| Error | Meaning |
|---|---|
| `ErrDaemonUnreachable` | A live record's endpoint could not be proved usable. |
| `*UnreachableError` | Carries `Record`, `Endpoint`, and the underlying handshake error. |
| `ErrExitUnconfirmed` | The process is gone but exit evidence is inconclusive. |
| `context.Canceled` / `context.DeadlineExceeded` | Caller bounds were reached while dialing, probing, starting, or stopping. |
| `daemon not running` | `Ensure` had no `Start` and discovery found nothing. |
| `daemon failed to start within ...` | Polling did not observe a compatible daemon before the deadline. |
| `dial daemon at ...` | Connecting to the endpoint failed. |
| `daemon handshake: ...` | The daemon answered with a protocol error (including proof configuration). |
| `daemon ping returned ok=false` | The handshake response was not OK. |
| `daemon proof mismatch` / `daemon proof response is missing or malformed` | Proof verification failed. |
| `daemon message size ... is invalid` / `malformed daemon message` | Framing or payload decoding failed. |
| `daemon lock path ... must be absolute` | Lock paths from options must be absolute. |
| `runtime dir ... must be absolute` | `RuntimeStore.Dir` must be absolute. |
| `runtime prefix ... must be a portable basename` | Invalid runtime prefix. |
| `prepare runtime dir: ...` | Private-directory preparation failed. |
| `validate unix socket dir: ...` | The socket directory is not private/current-user-owned. |
| `refusing to remove non-socket path ...` | A non-socket exists at the Unix socket path. |
| `daemon already listening on unix socket ...` | A live listener owns the path. |

When inspecting an error from discovery, prefer:

```go
if errors.Is(err, daemon.ErrDaemonUnreachable) {
	var unreachable *daemon.UnreachableError
	if errors.As(err, &unreachable) {
		// unreachable.Record, unreachable.Endpoint, unreachable.Err
	}
}
```

---

## Security notes

1. **Prove before you authorize.** If the daemon is configured with a `Proof`,
   require it in discovery and on application dials. Do not send application
   credentials until the handshake succeeds (`Manager.Find`/`Ensure`,
   `Proof.Probe`, or `DialerOptions.Proof`).
2. **Proof is opt-in.** Without a supplied `Proof`, the handshake is a plain
   ping, and a daemon without `PingHandlerOptions.Proof` refuses challenges. An
   unproven client can still ping an unproven daemon; that is a configuration
   choice, not an authorization decision.
3. **Proof is not application auth.** It proves possession of a shared key and
   binds the identity/endpoint; it does not authorize commands. Keep the
   application's own authorization in the application protocol.
4. **Treat unknown identity as no authority.** A record with an unreadable or
   foreign identity is kept and probed, not deleted or signaled. `Kill` does not
   verify identity; do that yourself.
5. **Runtime directories are private.** On Unix, `0700`, current-user-owned,
   no symlinks. On Windows, a protected DACL limited to the current user,
   `SYSTEM`, and built-in `Administrators`, with reparse points rejected.
6. **Runtime files are `0644`.** Do not put secrets in `Metadata`; the directory
   protects them, not the file mode.
7. **No public binds by default.** Use `RequireLoopback`/`RequireNonPublic` and
   keep TCP endpoints local.
8. **Unix socket cleanup is conservative.** Only a path that is a socket and
   probes as stale is removed, and only while holding the listen lock.
9. **The handshake preface is mandatory for application data.** A server that
   skips the `PingHandler` will read application bytes as a handshake frame, and
   a client that connects without `Endpoint.Dialer` must implement the handshake
   itself.

---

## Platform notes

### Unix (Linux, macOS, BSDs)

- Detached children use `Setsid`.
- File locks use `flock(2)` (`LOCK_EX`, `LOCK_NB`).
- Private directories are `0700`, current-user-owned, and opened with
  `O_NOFOLLOW` where ownership is repaired.
- Atomic writes fsync the staged file and the target directory.
- Stale Unix sockets are detected with `ECONNREFUSED`/`ENOENT` dial errors.
- Process identity:
  - Linux: `linux-v1:<boot-id>:<pid-namespace>:<start-ticks>`. A different boot
    ID proves the process gone. In the initial PID namespace, a foreign-namespace
    record is proven gone when no nested process matches its PID and start ticks.
  - macOS: start time from `kern.proc.pid` via `sysctl`.
  - BSDs: creation time is reported as unknown.
- Unix socket path limits: 104 bytes on macOS/BSD, 108 on Linux.

### Windows

- Detached children use `DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP`.
  **Do not** use `CREATE_NO_WINDOW` for daemons: hidden consoles expose
  `CONIN$`, and terminal-probing libraries can block forever.
- Liveness uses `GetExitCodeProcess` and requires `STILL_ACTIVE`; an openable
  process object alone is not proof of a running process (retained handles
  outlive termination).
- `Kill` uses `OpenProcess(PROCESS_TERMINATE)` + `TerminateProcess`.
- File locks use `LockFileEx`/`UnlockFileEx`.
- Private directories reject reparse points and enforce a protected DACL.
- Runtime files are opened with `FILE_FLAG_OPEN_REPARSE_POINT` and verified as
  regular, current-user-owned files.
- Atomic publish uses `MoveFileEx` with `REPLACE_EXISTING | WRITE_THROUGH`;
  directory fsync is a no-op because Windows has no equivalent.
- AF_UNIX sockets are supported; stale sockets report `WSAECONNREFUSED`.

### Fail-closed platforms

On Solaris, AIX, Plan 9, js/wasm, and wasip1 the package compiles, but
private-directory preparation, file locking, `Kill`, and process identity are
unavailable. `RuntimeStore` operations that need a private directory return an
error, and liveness checks report `false`/unknown. Do not build a daemon for
those platforms with this package.

---

## Development and testing

```bash
go test ./...
go vet ./...
gofmt -l .
```

Cross-target verification:

```bash
GOOS=darwin  GOARCH=arm64 go vet ./...
GOOS=windows GOARCH=amd64 go vet ./...
GOOS=freebsd GOARCH=amd64 go build ./...
```

The test suite uses local listeners, `net.Pipe` for handshake unit tests,
temporary directories, per-test runtime stores, and re-executed test binaries
for detached/kill/lock scenarios. It does not require elevated privileges or
external daemons, and it does not import `net/http`.

One Unix test (`TestListenUnixRejectsSharedSocketDirectoryEvenWithStoreLock`)
deliberately verifies that a socket in a shared directory is rejected. It
assumes the platform temp root (`/tmp`) is *not* a private `0700` directory. In
a sandbox where `/tmp` is already current-user-private, that test fails by
design; run tests with a normal shared `/tmp`.

Test-only helpers (for example `HoldLockForTest`, `EnsurePrivateDirForTest`,
`ErrNotPortable`, `StartLivePIDHelperForTest`) are exported from
`internal_test.go` and are not part of the public API.

---

## Package layout

| File | Responsibility |
|---|---|
| `endpoint.go` | Package doc, `Endpoint`, parsing/policies, `Dialer`, `Listen`, `DefaultSocketPath` |
| `runtime.go` | Runtime records/store, atomic writes, portable names |
| `lock.go` | Start/owner locks, cross-process file lock, local semaphore |
| `probe.go` | Handshake messages, ping handler, probe, discovery, proof |
| `daemon.go` | `Manager`, `StartDetached`, `Stop` |
| `process.go` | Identity types/scopes, liveness helpers, `Kill` |
| `process_linux.go` | Linux process identity |
| `process_unix_other.go` | macOS/BSD identity (Darwin sysctl; unknown elsewhere) |
| `platform_unix.go` | Unix detach/liveness/fs/locks/private dirs/socket errors |
| `platform_windows.go` | Windows equivalents, incl. DACLs and `LockFileEx` |
| `winapi_windows.go` | Raw Win32 bindings |
| `platform_other.go` | Fail-closed fallbacks for other platforms |

Tests are grouped by domain (`endpoint_test.go`, `runtime_test.go`,
`client_test.go`, `daemon_test.go`), by platform
(`platform_*_test.go`), and by package boundary (`internal_*_test.go` for
unexported behavior).

---

## Compatibility and upgrade notes

- The handshake is a new binary protocol (version `1`). It is **not**
  wire-compatible with the package's former HTTP-based ping. Upgrade servers and
  clients together.
- `Endpoint.Dialer` replaces the former `HTTPClient`; `DialerOptions` replaces
  `HTTPClientOptions`. The package no longer imports `net/http`.
- `Stop` no longer requires proof for TCP records. Supply `StopOptions.Proof`
  when the daemon uses proof; otherwise the handshake is a plain PID ping.
- Runtime record files and process-identity encodings are unchanged.
- Linux records use `process_identity_v2`. The legacy `process_identity` field
  is left empty on Linux so older clients treat the record as unverifiable
  rather than deleting it.
- macOS/Windows identities are Unix milliseconds and remain comparable across
  upgrades that keep the same encoding.
- A record with an identity this build cannot judge is **kept** and probed. After
  a downgrade, a dead newer daemon's record can therefore block `Ensure` with an
  `UnreachableError` until a caller that can prove the server gone removes it.
- Lock files intentionally persist. A leftover lock file with no holder does not
  block acquisition; the OS lock, not file existence, is authoritative.

---

## Troubleshooting

**`daemon handshake: proof is not configured`.**
The client sent a challenge but the daemon's ping handler has no `Proof`.
Configure `PingHandlerOptions.Proof` on the server or stop sending
`DialerOptions.Proof`/`DiscoverOptions.Proof` from the client.

**`daemon proof mismatch` or `daemon proof response is missing or malformed`.**
The client and server have different tokens, or a middlebox/relay rewrote the
identity. Ensure both sides use the same key and that `RuntimeRecord.Address`
is the address clients actually dial (the proof binds it).

**`daemon ping returned ok=false` / `daemon handshake: ...`.**
The daemon answered but rejected the request (protocol version, malformed
challenge, missing proof configuration, or invalid record identity). Check both
sides run the same package version.

**The application protocol reads garbage.**
The server must run the handshake handler before reading application data. If
you bypass `Endpoint.Dialer` on the client, implement the same preface; the
server's first read is the handshake frame.

**`daemon is unreachable` although the PID is alive.**
The endpoint could not be proved (transport failure, handshake error, wrong
service, or failed proof). Inspect `UnreachableError.Endpoint` and `.Err`.

**`Ensure` returns `ErrDaemonUnreachable` and does not start a daemon.**
That is intentional: a live record exists but cannot be verified, so starting a
second daemon could corrupt application state. Fix or remove the stale record
with a mechanism that can prove the old server gone.

**`Ensure` times out while another caller holds the start lock.**
Contention is authoritative. Ensure the other starter completes, or bound the
work with your context. Do not delete the lock file: it is the same filesystem
object every holder uses, and only its OS lock state matters.

**`Unix socket path ... exceeds platform limit`.**
Use `DefaultSocketPath` (short per-user path) or place the socket in a shorter
private directory. Limits are 104 bytes on macOS/BSD and 108 on Linux/Windows.

**`validate unix socket dir` / `prepare runtime dir` errors.**
The directory must be absolute, current-user-owned, non-symlink, and private
(`0700` on Unix; protected DACL on Windows). Let the package create it rather
than pointing at a shared directory.

**`refusing to remove non-socket path`.**
Something else is at the socket path. Move or delete it yourself; the package
will not remove a path it cannot identify as a stale socket.

**`Stop` returns `ErrExitUnconfirmed`.**
The process is gone but exit evidence was inconclusive. Supply
`StopOptions.Exited` with application-specific evidence (for example, acquiring
the daemon's ownership lock or observing a state file removal).

**Auto-start refuses to launch.**
If `RefuseEphemeral` is set, `go run`/go-build and `*.test` binaries are
rejected. Build a real binary first. On Windows, ensure you use
`StartDetached` (not a hidden console) so terminal probes cannot block.

**A process was killed but its `ProcessAlive` still reports true on Windows.**
`ProcessAlive` checks the exit status (`STILL_ACTIVE`), not just handle
openability. If you observe otherwise, the PID was likely reused; verify
creation identity before signaling or deleting records.
