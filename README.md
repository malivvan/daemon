# daemon [![godoc](https://godoc.org/github.com/malivvan/daemon?status.svg)](https://godoc.org/github.com/malivvan/daemon) ![test](https://github.com/malivvan/daemon/workflows/test/badge.svg) [![Coverage Status](https://coveralls.io/repos/github/malivvan/daemon/badge.svg?branch=master)](https://coveralls.io/github/malivvan/daemon?branch=master) [![Release](https://img.shields.io/github/v/release/malivvan/daemon.svg?sort=semver)](https://github.com/malivvan/daemon/releases/latest) [![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A small Go package for running a process in the background and for protecting shared resources with a pid-based lockfile.

## Overview

This repository exposes two related utilities:

- `daemon.Start`, `Stop`, `Kill`, and `Wait` for daemonizing or managing a background process.
- `Lockfile` and related helpers for cooperative locking around a file on disk.

The package is intentionally small and relies on standard library primitives. It supports Unix-like systems and Windows with separate implementation files for each platform.

## Installation

```bash
go get github.com/malivvan/daemon
```

Then import it as:

```go
import "github.com/malivvan/daemon"
```

## Daemon usage

On Unix-like platforms, `Start` forks a detached child process by re-launching the current executable with the provided arguments. The first call runs in the parent, the second call is the intermediate child, and the final process is the daemonized instance. The package tracks the state using an environment variable named `__DAEMON_STAGE`.

```go
package main

import (
    "fmt"
    "os"
    "github.com/malivvan/daemon"
)

func main() {
    pid, err := daemon.Start([]string{"serve"})
    if err != nil {
        panic(err)
    }

    fmt.Println("daemon pid:", pid)

    // Use the daemon just as a background process.
    // When done:
    if err := daemon.Stop(pid); err != nil {
        fmt.Println("stop failed:", err)
    }

    // Or force terminate with:
    // _ = daemon.Kill(pid)

    // Wait for the daemon to exit.
    daemon.Wait(pid)

    _, _ = os.Stdout.WriteString("done\n")
}
```

### Process controls

The package provides these process helpers:

- `Start(args []string) (int, error)`
  - Starts a detached/background process.
  - On Unix, it detaches the process from the controlling terminal using `setsid`.
  - On Windows, it starts a new detached process without inheriting the current console.

- `Stop(pid int) error`
  - Sends an interrupt-style signal to the process.
  - On Unix this is `SIGINT`; on Windows it calls `GenerateConsoleCtrlEvent`.

- `Kill(pid int) error`
  - Terminates the process immediately.
  - On Unix this is `SIGKILL`; on Windows it uses `TerminateProcess`.

- `Wait(pid int)`
  - Blocks until the child process exits, using `os.Process.Wait`.

- `Stage() int`
  - Reports the daemon stage for the current process when relevant.

## Lockfile usage

The `Lockfile` type is a pid file that can be acquired exclusively. It is designed for inter-process synchronization. A lock must be created with an absolute path.

```go
package main

import (
    "fmt"
    "os"
    "path/filepath"

    "github.com/malivvan/daemon"
)

func main() {
    lockPath := filepath.Join(os.TempDir(), "myapp.lock")
    lock, err := daemon.NewLockfile(lockPath)
    if err != nil {
        panic(err)
    }

    if err := lock.TryLock(); err != nil {
        panic(err)
    }
    defer func() {
        if err := lock.Unlock(); err != nil {
            panic(err)
        }
    }()

    fmt.Println("do work under lock")
}
```

### Lockfile behavior

`NewLockfile(path string)` validates that the path is absolute and returns a `Lockfile` value.

`TryLock()` attempts to acquire the lock by:

- creating a temporary pid file with the current process ID,
- linking it into the target lock path,
- checking whether the lock file is valid and whether it is already owned by another running process,
- removing stale or invalid lockfiles before retrying.

`Unlock()` removes the lock only if it is still owned by the current process. If the lock file was unexpectedly deleted or the owner is not this process, it returns `ErrRogueDeletion`.

### Errors returned by the lockfile API

The package exposes these errors and temporary errors:

- `ErrBusy` – lock is already held by another process
- `ErrNotExist` – lockfile was created but disappeared during the locking sequence
- `ErrNeedAbsPath` – lockfile path must be absolute
- `ErrInvalidPid` – data in the lockfile is not a valid positive PID
- `ErrDeadOwner` – lockfile points to a PID that no longer exists
- `ErrRogueDeletion` – lockfile ownership does not match the current process or was removed unexpectedly

`TemporaryError` implements `Temporary() bool`, so callers can treat these as retriable errors.

## Notes and caveats

- The daemon implementation is intentionally minimal and assumes the process can re-exec itself using `_path` from `os.Args[0]`.
- The lockfile is pid-based, not a full advisory file lock; it relies on process existence checks and file-link semantics.
- On Unix, `Stop` is a graceful interrupt-style stop, not a guaranteed clean shutdown for arbitrary child processes.
- On Windows, the package uses the Windows API to create a detached process and terminate it when requested.

## License

This project is released under the MIT license.

