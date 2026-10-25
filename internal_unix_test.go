//go:build !windows

package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDetachChildStartsNewSession(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "/bin/sh")
	detachChild(cmd)

	assert.True(t, cmd.SysProcAttr.Setsid, "child must start its own session")
}

func TestUnixSocketStaleTreatsMissingSocketAsStale(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "kitd-probe") //nolint:usetesting // unix socket paths must stay short, so the test needs a fixed OS temp root
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	stale, err := unixSocketStale(t.Context(), filepath.Join(dir, "missing.sock"), 50*time.Millisecond)

	require.NoError(t, err)
	assert.True(t, stale)
}
