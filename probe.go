package daemon

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"net/http"
	"os"
	"strconv"
	"time"
)

// DefaultPingPath is the conventional liveness endpoint used by Probe.
const DefaultPingPath = "/api/ping"

// PingInfo is the common identity returned by daemon ping endpoints. OK must
// be true for a probe to succeed.
type PingInfo struct {
	OK      bool   `json:"ok"`
	Service string `json:"service,omitempty"`
	Version string `json:"version,omitempty"`
	PID     int    `json:"pid,omitempty"`
}

// PingHandlerOptions configures NewPingHandler.
type PingHandlerOptions struct {
	Service string
	Version string
	PID     int
}

// NewPingHandler returns a reference handler for DefaultPingPath-compatible
// daemon liveness endpoints.
func NewPingHandler(opts PingHandlerOptions) http.Handler {
	pid := opts.PID
	if pid == 0 {
		pid = os.Getpid()
	}
	info := PingInfo{
		OK:      true,
		Service: opts.Service,
		Version: opts.Version,
		PID:     pid,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(info)
	})
}

// ProbeOptions configures daemon liveness probes.
type ProbeOptions struct {
	Path            string
	ExpectedService string
	Timeout         time.Duration
}

func (o ProbeOptions) path() string {
	if o.Path == "" {
		return DefaultPingPath
	}
	return o.Path
}

func (o ProbeOptions) timeout() time.Duration {
	if o.Timeout == 0 {
		return time.Second
	}
	return o.Timeout
}

// Probe checks that ep answers its ping endpoint.
func Probe(ctx context.Context, ep Endpoint, opts ProbeOptions) (PingInfo, error) {
	if ep.Address == "" {
		return PingInfo{}, errors.New("empty daemon endpoint address")
	}
	client := ep.HTTPClient(HTTPClientOptions{
		Timeout:           opts.timeout(),
		DisableKeepAlives: true,
	})
	return ProbeHTTP(ctx, client, ep.BaseURL(), opts)
}

func probeRecordEndpoint(ctx context.Context, rec RuntimeRecord, proof *Proof, opts ProbeOptions) (PingInfo, error) {
	if proof != nil {
		return proof.Probe(ctx, rec, opts)
	}
	return Probe(ctx, rec.Endpoint(), opts)
}

// ProbeHTTP checks that baseURL answers its ping endpoint.
func ProbeHTTP(ctx context.Context, client *http.Client, baseURL string, opts ProbeOptions) (PingInfo, error) {
	info, _, err := probeHTTP(ctx, client, baseURL, opts, "")
	return info, err
}

func probeHTTP(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	opts ProbeOptions,
	proofChallenge string,
) (PingInfo, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, opts.timeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+opts.path(), nil)
	if err != nil {
		return PingInfo{}, nil, err
	}
	if proofChallenge != "" {
		req.Header.Set(proofChallengeHeader, proofChallenge)
	}
	resp, err := client.Do(req)
	if err != nil {
		return PingInfo{}, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return PingInfo{}, nil, fmt.Errorf("daemon ping returned %d", resp.StatusCode)
	}
	var info PingInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return PingInfo{}, nil, fmt.Errorf("decode daemon ping: %w", err)
	}
	if !info.OK {
		return PingInfo{}, nil, errors.New("daemon ping returned ok=false")
	}
	if opts.ExpectedService != "" && info.Service != opts.ExpectedService {
		return PingInfo{}, nil, fmt.Errorf("unexpected daemon service %q", info.Service)
	}
	return info, resp.Header.Clone(), nil
}

// DiscoverOptions configures runtime-file discovery.
type DiscoverOptions struct {
	Probe ProbeOptions
	// Proof enables proof-of-possession probing for each runtime record.
	Proof *Proof
	// RequirePIDAlive rejects dead processes and definite process-identity
	// mismatches. A record whose identity this caller can't judge is kept and
	// probed (see AGENTS.md). It also makes failed probes indeterminate:
	// Discover returns an UnreachableError when no later record succeeds. The
	// false zero value keeps the earlier compatibility behavior, where failed
	// probes are skipped and discovery can report definite absence.
	RequirePIDAlive bool
	Accept          func(RuntimeRecord, PingInfo) bool
}

// ErrDaemonUnreachable reports a live runtime record whose endpoint could not
// be proved usable. It classifies every Probe or proof-of-possession failure,
// including transport, HTTP, decoding, service-identity, and proof errors.
// Callers should inspect the wrapped error before choosing operator guidance.
var ErrDaemonUnreachable = errors.New("daemon is unreachable")

// UnreachableError retains the runtime and probe failure for a live daemon
// candidate that discovery could not safely select.
type UnreachableError struct {
	Record   RuntimeRecord
	Endpoint Endpoint
	Err      error
}

func (e *UnreachableError) Error() string {
	return fmt.Sprintf("%v: pid %d at %s: %v",
		ErrDaemonUnreachable, e.Record.PID, e.Endpoint.ConfigAddress(), e.Err)
}

// Unwrap preserves both the classification and the endpoint probe failure.
func (e *UnreachableError) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrDaemonUnreachable}
	}
	return []error{ErrDaemonUnreachable, e.Err}
}

// Discover scans runtime records and returns the first live daemon.
func Discover(ctx context.Context, store RuntimeStore, opts DiscoverOptions) (RuntimeRecord, PingInfo, bool, error) {
	if err := ctx.Err(); err != nil {
		return RuntimeRecord{}, PingInfo{}, false, err
	}
	records, err := store.List()
	if err != nil {
		return RuntimeRecord{}, PingInfo{}, false, err
	}
	var unreachable error
	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return RuntimeRecord{}, PingInfo{}, false, err
		}
		if opts.RequirePIDAlive {
			if !runtimeRecordLive(rec) {
				continue
			}
		}
		ep := rec.Endpoint()
		info, err := probeRecordEndpoint(ctx, rec, opts.Proof, opts.Probe)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return RuntimeRecord{}, PingInfo{}, false, ctxErr
			}
			if opts.RequirePIDAlive && unreachable == nil {
				unreachable = &UnreachableError{
					Record:   rec,
					Endpoint: ep,
					Err:      err,
				}
			}
			continue
		}
		if opts.RequirePIDAlive && info.PID != rec.PID {
			continue
		}
		if opts.Accept != nil && !opts.Accept(rec, info) {
			continue
		}
		return rec, info, true, nil
	}
	return RuntimeRecord{}, PingInfo{}, false, unreachable
}

const (
	proofNonceSize       = 32
	proofChallengeHeader = "X-Kit-Daemon-Challenge"
	proofResponseHeader  = "X-Kit-Daemon-Proof"
	proofDomain          = "go.kenn.io/kit/daemon/proof/v1"
)

var proofEncoding = base64.RawURLEncoding.Strict()

// Proof owns the shared secret used to prove a daemon runtime identity without
// sending the secret to a candidate endpoint. Construct one with NewProof.
type Proof struct { //nolint:recvcheck // Format keeps a value receiver so both Proof and *Proof redact when printed
	key []byte
}

// NewProof copies key into an opaque proof capability.
func NewProof(key []byte) (*Proof, error) {
	if len(key) == 0 {
		return nil, errors.New("daemon proof key is empty")
	}
	return &Proof{key: bytes.Clone(key)}, nil
}

// Format redacts the proof key from diagnostic formatting.
func (Proof) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("<daemon proof>"))
}

// NewPingHandler returns a standard ping handler that proves possession of
// the proof key when challenged. Unchallenged requests retain the ordinary
// PingInfo readiness response.
func (p *Proof) NewPingHandler(rec RuntimeRecord) (http.Handler, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if _, err := proofIdentityForRecord(rec); err != nil {
		return nil, err
	}
	ping := NewPingHandler(PingHandlerOptions{
		Service: rec.Service,
		Version: rec.Version,
		PID:     rec.PID,
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		challenges := r.Header.Values(proofChallengeHeader)
		if r.Method == http.MethodGet && len(challenges) > 0 &&
			!p.writePingProof(w, challenges, rec) {
			return
		}
		ping.ServeHTTP(w, r)
	}), nil
}

func (p *Proof) writePingProof(w http.ResponseWriter, challenges []string, rec RuntimeRecord) bool {
	if len(challenges) != 1 {
		http.Error(w, "invalid daemon proof challenge", http.StatusBadRequest)
		return false
	}
	nonce, err := decodeProofValue(challenges[0], proofNonceSize)
	if err != nil {
		http.Error(w, "invalid daemon proof challenge", http.StatusBadRequest)
		return false
	}
	proof, err := proofMAC(p.key, nonce, rec)
	if err != nil {
		http.Error(w, "daemon proof unavailable", http.StatusInternalServerError)
		return false
	}
	w.Header().Set(proofResponseHeader, proofEncoding.EncodeToString(proof))
	return true
}

// Probe checks that the endpoint in rec answers its ping endpoint and proves
// possession of the proof key for the runtime identity before returning.
func (p *Proof) Probe(ctx context.Context, rec RuntimeRecord, opts ProbeOptions) (PingInfo, error) {
	if err := p.validate(); err != nil {
		return PingInfo{}, err
	}
	ep := rec.Endpoint()
	if ep.Address == "" {
		return PingInfo{}, errors.New("empty daemon endpoint address")
	}
	client := ep.HTTPClient(HTTPClientOptions{
		Timeout:           opts.timeout(),
		DisableKeepAlives: true,
	})
	return p.probeHTTP(ctx, client, ep.BaseURL(), rec, opts)
}

func (p *Proof) probeHTTP(
	ctx context.Context,
	client *http.Client,
	baseURL string,
	rec RuntimeRecord,
	opts ProbeOptions,
) (PingInfo, error) {
	if err := p.validate(); err != nil {
		return PingInfo{}, err
	}
	if _, err := proofIdentityForRecord(rec); err != nil {
		return PingInfo{}, err
	}
	nonce := make([]byte, proofNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return PingInfo{}, fmt.Errorf("generate daemon proof challenge: %w", err)
	}
	challenge := proofEncoding.EncodeToString(nonce)
	info, headers, err := probeHTTP(ctx, client, baseURL, opts, challenge)
	if err != nil {
		return PingInfo{}, err
	}
	proofs := headers.Values(proofResponseHeader)
	if len(proofs) != 1 {
		return PingInfo{}, errors.New("daemon proof response is missing or repeated")
	}
	proof, err := decodeProofValue(proofs[0], sha256.Size)
	if err != nil {
		return PingInfo{}, errors.New("daemon proof response is malformed")
	}
	want, err := proofMAC(p.key, nonce, rec)
	if err != nil {
		return PingInfo{}, err
	}
	if !hmac.Equal(proof, want) {
		return PingInfo{}, errors.New("daemon proof mismatch")
	}
	if info.Service != rec.Service || info.Version != rec.Version || info.PID != rec.PID {
		return PingInfo{}, errors.New("daemon proof identity does not match ping response")
	}
	return info, nil
}

func (p *Proof) validate() error {
	if p == nil || len(p.key) == 0 {
		return errors.New("daemon proof is not initialized")
	}
	return nil
}

type proofIdentity struct {
	service string
	version string
	pid     string
	network string
	address string
}

func proofIdentityForRecord(rec RuntimeRecord) (proofIdentity, error) {
	if rec.Service == "" {
		return proofIdentity{}, errors.New("daemon proof service is empty")
	}
	if rec.PID <= 0 {
		return proofIdentity{}, errors.New("daemon proof PID must be positive")
	}
	ep := rec.Endpoint()
	if ep.Network != NetworkTCP && ep.Network != NetworkUnix {
		return proofIdentity{}, fmt.Errorf("unsupported daemon proof network %q", ep.Network)
	}
	if ep.Address == "" {
		return proofIdentity{}, errors.New("daemon proof address is empty")
	}
	return proofIdentity{
		service: rec.Service,
		version: rec.Version,
		pid:     strconv.Itoa(rec.PID),
		network: ep.Network,
		address: ep.Address,
	}, nil
}

func proofMAC(key, nonce []byte, rec RuntimeRecord) ([]byte, error) {
	if len(key) == 0 {
		return nil, errors.New("daemon proof key is empty")
	}
	if len(nonce) != proofNonceSize {
		return nil, errors.New("daemon proof nonce has invalid length")
	}
	identity, err := proofIdentityForRecord(rec)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key)
	writeProofField(mac, []byte(proofDomain))
	writeProofField(mac, nonce)
	writeProofField(mac, []byte(identity.service))
	writeProofField(mac, []byte(identity.version))
	writeProofField(mac, []byte(identity.pid))
	writeProofField(mac, []byte(identity.network))
	writeProofField(mac, []byte(identity.address))
	return mac.Sum(nil), nil
}

func writeProofField(mac hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = mac.Write(size[:])
	_, _ = mac.Write(value)
}

func decodeProofValue(value string, size int) ([]byte, error) {
	decoded, err := proofEncoding.DecodeString(value)
	if err != nil || len(decoded) != size {
		return nil, errors.New("invalid daemon proof value")
	}
	return decoded, nil
}
