//go:build linux

package daemon_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/malivvan/daemon"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLinuxRuntimeRecordUsesVersionedIdentityWithoutExposingItToOldClients(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	rec := daemon.NewRuntimeRecord("tool", "v1", daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1234",
	})
	require.NotEmpty(rec.ProcessIdentityV2)
	assert.Empty(rec.ProcessIdentity)
	assert.Equal(daemon.ProcessIdentityMatch, daemon.CompareRuntimeProcessIdentity(rec))

	body, err := json.Marshal(rec)
	require.NoError(err)
	var legacy struct {
		ProcessIdentity daemon.ProcessIdentity `json:"process_identity"`
	}
	require.NoError(json.Unmarshal(body, &legacy))
	assert.Empty(legacy.ProcessIdentity)
}

func TestLinuxLegacyWallClockIdentityFailsClosed(t *testing.T) {
	rec := daemon.RuntimeRecord{
		PID:             1,
		ProcessIdentity: "123456789",
	}
	assert.Equal(t, daemon.ProcessIdentityUnknown, daemon.CompareRuntimeProcessIdentity(rec))
}

func TestRuntimeProcessAliveIgnoresLocalPIDWhenIdentityProvesProcessGone(t *testing.T) {
	own := daemon.NewRuntimeRecord("tool", "v1", daemon.Endpoint{})
	rec := withLinuxIdentityField(t, own, 1, "11111111-2222-3333-4444-555555555555")
	assert.False(t, daemon.RuntimeProcessAlive(rec))
}

func TestStopUnjudgedIdentityWithDeadLocalPID(t *testing.T) {
	pid := deadPID(t)
	proof := newDaemonProof(t, []byte("stop-secret-key"))
	rec := daemon.RuntimeRecord{PID: pid, Service: "test", ProcessIdentityV2: "future-v1:12345"}
	server, rec := startProofServer(t, rec, proof)
	defer server.Close()
	rec = storedStopRecord(t, rec)
	requests := 0
	err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
		Proof:   proof,
		Timeout: time.Second,
		Request: func(_ context.Context, got daemon.RuntimeRecord) error {
			assert.Equal(t, rec, got)
			requests++
			server.Close()
			return nil
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, requests)
}
