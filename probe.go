package daemon

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"os"
	"strconv"
	"time"
)

const (
	// wireVersion is the daemon handshake protocol version.
	wireVersion uint8 = 1

	messagePingRequest  uint8 = 1
	messagePingResponse uint8 = 2

	// maxMessageSize bounds a single framed handshake message.
	maxMessageSize = 1 << 20

	proofNonceSize = 32
	proofDomain    = "go.kenn.io/kit/daemon/proof/v1"
)

// PingInfo is the identity returned by the daemon handshake. OK must be true
// for a probe to succeed.
type PingInfo struct {
	OK      bool   `json:"ok"`
	Service string `json:"service,omitempty"`
	Version string `json:"version,omitempty"`
	PID     int    `json:"pid,omitempty"`
}

// PingRequest is the client's handshake message. Challenge is empty for an
// unproven ping and a proof nonce when the client wants proof of possession.
type PingRequest struct {
	Challenge []byte
}

// PingResponse is the daemon's handshake reply. Proof is present only when the
// request carried a challenge and the daemon has a proof key.
type PingResponse struct {
	Info  PingInfo
	Proof []byte
	Error string
}

// Encode serializes the request without external dependencies.
//
// Layout: message type, protocol version, 2-byte challenge length, challenge.
func (m PingRequest) Encode() ([]byte, error) {
	out := make([]byte, 0, 4+len(m.Challenge))
	out = append(out, messagePingRequest, wireVersion)
	out, err := appendWireBytes(out, m.Challenge)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Decode parses an encoded PingRequest and rejects malformed input.
func (m *PingRequest) Decode(data []byte) error {
	r := &wireReader{data: data}
	if err := r.header(messagePingRequest); err != nil {
		return err
	}
	challenge, err := r.bytes()
	if err != nil {
		return err
	}
	if !r.done() {
		return fmt.Errorf("%w: trailing request data", errMalformedMessage)
	}
	m.Challenge = challenge
	return nil
}

// Encode serializes the response without external dependencies.
//
// Layout: message type, protocol version, status flags, service, version,
// 8-byte PID, proof, error message.
func (m PingResponse) Encode() ([]byte, error) {
	out := make([]byte, 0, 64+len(m.Info.Service)+len(m.Info.Version)+len(m.Proof)+len(m.Error))
	out = append(out, messagePingResponse, wireVersion)
	var flags uint8
	if m.Info.OK {
		flags = 1
	}
	out = append(out, flags)
	var err error
	if out, err = appendWireString(out, m.Info.Service); err != nil {
		return nil, err
	}
	if out, err = appendWireString(out, m.Info.Version); err != nil {
		return nil, err
	}
	var pid [8]byte
	binary.BigEndian.PutUint64(pid[:], uint64(int64(m.Info.PID)))
	out = append(out, pid[:]...)
	if out, err = appendWireBytes(out, m.Proof); err != nil {
		return nil, err
	}
	if out, err = appendWireString(out, m.Error); err != nil {
		return nil, err
	}
	return out, nil
}

// Decode parses an encoded PingResponse and rejects malformed input.
func (m *PingResponse) Decode(data []byte) error {
	r := &wireReader{data: data}
	if err := r.header(messagePingResponse); err != nil {
		return err
	}
	flags, err := r.u8()
	if err != nil {
		return err
	}
	service, err := r.string()
	if err != nil {
		return err
	}
	version, err := r.string()
	if err != nil {
		return err
	}
	pid, err := r.u64()
	if err != nil {
		return err
	}
	proof, err := r.bytes()
	if err != nil {
		return err
	}
	errorText, err := r.string()
	if err != nil {
		return err
	}
	if !r.done() {
		return fmt.Errorf("%w: trailing response data", errMalformedMessage)
	}
	*m = PingResponse{
		Info: PingInfo{
			OK:      flags&1 != 0,
			Service: service,
			Version: version,
			PID:     int(int64(pid)),
		},
		Proof: proof,
		Error: errorText,
	}
	return nil
}

var errMalformedMessage = errors.New("malformed daemon message")

// wireReader decodes the primitive fields of a handshake message.
type wireReader struct {
	data []byte
	off  int
}

func (r *wireReader) header(wantType uint8) error {
	messageType, err := r.u8()
	if err != nil {
		return err
	}
	if messageType != wantType {
		return fmt.Errorf("%w: unexpected message type %d", errMalformedMessage, messageType)
	}
	version, err := r.u8()
	if err != nil {
		return err
	}
	if version != wireVersion {
		return fmt.Errorf("%w: unsupported protocol version %d", errMalformedMessage, version)
	}
	return nil
}

func (r *wireReader) u8() (uint8, error) {
	if r.off+1 > len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}
	value := r.data[r.off]
	r.off++
	return value, nil
}

func (r *wireReader) u64() (uint64, error) {
	if r.off+8 > len(r.data) {
		return 0, io.ErrUnexpectedEOF
	}
	value := binary.BigEndian.Uint64(r.data[r.off:])
	r.off += 8
	return value, nil
}

func (r *wireReader) bytes() ([]byte, error) {
	if r.off+2 > len(r.data) {
		return nil, io.ErrUnexpectedEOF
	}
	length := int(binary.BigEndian.Uint16(r.data[r.off:]))
	r.off += 2
	if r.off+length > len(r.data) {
		return nil, io.ErrUnexpectedEOF
	}
	value := bytes.Clone(r.data[r.off : r.off+length])
	r.off += length
	return value, nil
}

func (r *wireReader) string() (string, error) {
	value, err := r.bytes()
	if err != nil {
		return "", err
	}
	return string(value), nil
}

func (r *wireReader) done() bool {
	return r.off == len(r.data)
}

func appendWireString(dst []byte, value string) ([]byte, error) {
	if len(value) > 0xffff {
		return nil, fmt.Errorf("daemon message string is %d bytes", len(value))
	}
	var size [2]byte
	binary.BigEndian.PutUint16(size[:], uint16(len(value)))
	dst = append(dst, size[:]...)
	return append(dst, value...), nil
}

func appendWireBytes(dst []byte, value []byte) ([]byte, error) {
	if len(value) > 0xffff {
		return nil, fmt.Errorf("daemon message field is %d bytes", len(value))
	}
	var size [2]byte
	binary.BigEndian.PutUint16(size[:], uint16(len(value)))
	dst = append(dst, size[:]...)
	return append(dst, value...), nil
}

// PingHandlerOptions configures a daemon handshake responder.
type PingHandlerOptions struct {
	// Record is the identity announced to clients and bound by proof. A zero
	// PID becomes the current process's PID.
	Record RuntimeRecord
	// Proof, when set, lets the handler answer proof challenges. Nil makes it
	// reject challenged handshakes.
	Proof *Proof
}

// PingHandler answers the daemon handshake on one accepted connection. It does
// not close the connection: the caller owns it and may continue with its own
// protocol after the handshake returns.
type PingHandler func(ctx context.Context, conn net.Conn) error

// NewPingHandler returns a handshake handler for rec.
func NewPingHandler(opts PingHandlerOptions) PingHandler {
	rec := opts.Record
	if rec.PID == 0 {
		rec.PID = os.Getpid()
	}
	info := PingInfo{
		OK:      true,
		Service: rec.Service,
		Version: rec.Version,
		PID:     rec.PID,
	}
	endpoint := rec.Endpoint()
	return func(ctx context.Context, conn net.Conn) error {
		if deadline, ok := ctx.Deadline(); ok {
			_ = conn.SetDeadline(deadline)
		}
		defer func() { _ = conn.SetDeadline(time.Time{}) }()

		payload, err := readMessage(conn)
		if err != nil {
			return fmt.Errorf("read daemon handshake: %w", err)
		}
		var req PingRequest
		if err := req.Decode(payload); err != nil {
			return writeMessage(conn, PingResponse{Error: err.Error()})
		}
		resp := PingResponse{Info: info}
		switch {
		case len(req.Challenge) == 0:
			return writeMessage(conn, resp)
		case opts.Proof == nil:
			resp.Info.OK = false
			resp.Error = "daemon proof is not configured"
		case len(req.Challenge) != proofNonceSize:
			resp.Info.OK = false
			resp.Error = "invalid daemon proof challenge"
		default:
			mac, err := proofMAC(opts.Proof.key, req.Challenge, endpoint, info)
			if err != nil {
				resp.Info.OK = false
				resp.Error = err.Error()
				break
			}
			resp.Proof = mac
		}
		return writeMessage(conn, resp)
	}
}

// ProbeOptions configures daemon liveness probes.
type ProbeOptions struct {
	ExpectedService string
	Timeout         time.Duration
}

func (o ProbeOptions) timeout() time.Duration {
	if o.Timeout == 0 {
		return time.Second
	}
	return o.Timeout
}

// Probe checks that ep answers the daemon handshake. It does not prove
// possession of a proof key; use Proof.Probe or DiscoverOptions.Proof for that.
func Probe(ctx context.Context, ep Endpoint, opts ProbeOptions) (PingInfo, error) {
	conn, info, err := dialEndpoint(ctx, ep, DialerOptions{Timeout: opts.timeout()})
	if err != nil {
		return PingInfo{}, err
	}
	_ = conn.Close()
	if opts.ExpectedService != "" && info.Service != opts.ExpectedService {
		return PingInfo{}, fmt.Errorf("unexpected daemon service %q", info.Service)
	}
	return info, nil
}

func probeRecordEndpoint(ctx context.Context, rec RuntimeRecord, proof *Proof, opts ProbeOptions) (PingInfo, error) {
	if proof != nil {
		return proof.Probe(ctx, rec, opts)
	}
	return Probe(ctx, rec.Endpoint(), opts)
}

// DiscoverOptions configures runtime-file discovery.
type DiscoverOptions struct {
	Probe ProbeOptions
	// Proof, when set, requires proof of possession for each probed record.
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
// be proved usable. It classifies every probe or proof-of-possession failure,
// including transport, handshake, service-identity, and proof errors.
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

// NewPingHandler returns a handshake handler that answers proof challenges for
// rec. Unchallenged handshakes retain the ordinary readiness response.
func (p *Proof) NewPingHandler(rec RuntimeRecord) (PingHandler, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	if _, err := proofMAC(p.key, make([]byte, proofNonceSize), rec.Endpoint(), pingInfoForRecord(rec)); err != nil {
		return nil, err
	}
	return NewPingHandler(PingHandlerOptions{Record: rec, Proof: p}), nil
}

// Probe checks that the endpoint in rec answers the daemon handshake and proves
// possession of the proof key for the runtime identity before returning.
func (p *Proof) Probe(ctx context.Context, rec RuntimeRecord, opts ProbeOptions) (PingInfo, error) {
	if err := p.validate(); err != nil {
		return PingInfo{}, err
	}
	ep := rec.Endpoint()
	conn, info, err := dialEndpoint(ctx, ep, DialerOptions{Timeout: opts.timeout(), Proof: p})
	if err != nil {
		return PingInfo{}, err
	}
	_ = conn.Close()
	if info.Service != rec.Service || info.Version != rec.Version || info.PID != rec.PID {
		return PingInfo{}, errors.New("daemon proof identity does not match ping response")
	}
	if opts.ExpectedService != "" && info.Service != opts.ExpectedService {
		return PingInfo{}, fmt.Errorf("unexpected daemon service %q", info.Service)
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

func pingInfoForRecord(rec RuntimeRecord) PingInfo {
	return PingInfo{
		OK:      true,
		Service: rec.Service,
		Version: rec.Version,
		PID:     rec.PID,
	}
}

func proofIdentityFor(ep Endpoint, info PingInfo) (proofIdentity, error) {
	if info.Service == "" {
		return proofIdentity{}, errors.New("daemon proof service is empty")
	}
	if info.PID <= 0 {
		return proofIdentity{}, errors.New("daemon proof PID must be positive")
	}
	if ep.Network != NetworkTCP && ep.Network != NetworkUnix {
		return proofIdentity{}, fmt.Errorf("unsupported daemon proof network %q", ep.Network)
	}
	if ep.Address == "" {
		return proofIdentity{}, errors.New("daemon proof address is empty")
	}
	return proofIdentity{
		service: info.Service,
		version: info.Version,
		pid:     strconv.Itoa(info.PID),
		network: ep.Network,
		address: ep.Address,
	}, nil
}

func proofMAC(key, nonce []byte, ep Endpoint, info PingInfo) ([]byte, error) {
	if len(key) == 0 {
		return nil, errors.New("daemon proof key is empty")
	}
	if len(nonce) != proofNonceSize {
		return nil, errors.New("daemon proof nonce has invalid length")
	}
	identity, err := proofIdentityFor(ep, info)
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

// dialEndpoint connects and performs the daemon handshake, returning a
// connection positioned for the application's protocol.
func dialEndpoint(ctx context.Context, ep Endpoint, opts DialerOptions) (net.Conn, PingInfo, error) {
	if ep.Network == "" {
		return nil, PingInfo{}, errors.New("empty daemon endpoint network")
	}
	if ep.Address == "" {
		return nil, PingInfo{}, errors.New("empty daemon endpoint address")
	}
	if opts.Proof != nil {
		if err := opts.Proof.validate(); err != nil {
			return nil, PingInfo{}, err
		}
	}
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	dialer := &net.Dialer{KeepAlive: opts.KeepAlive}
	conn, err := dialer.DialContext(ctx, ep.Network, ep.Address)
	if err != nil {
		return nil, PingInfo{}, fmt.Errorf("dial daemon at %s: %w", ep.ConfigAddress(), err)
	}
	info, err := pingConn(ctx, conn, ep, opts.Proof)
	if err != nil {
		_ = conn.Close()
		return nil, PingInfo{}, err
	}
	return conn, info, nil
}

// pingConn performs the client side of the handshake on an open connection.
func pingConn(ctx context.Context, conn net.Conn, ep Endpoint, proof *Proof) (PingInfo, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	defer func() { _ = conn.SetDeadline(time.Time{}) }()

	req := PingRequest{}
	var nonce []byte
	if proof != nil {
		if err := proof.validate(); err != nil {
			return PingInfo{}, err
		}
		nonce = make([]byte, proofNonceSize)
		if _, err := rand.Read(nonce); err != nil {
			return PingInfo{}, fmt.Errorf("generate daemon proof challenge: %w", err)
		}
		req.Challenge = nonce
	}
	if err := writeMessage(conn, req); err != nil {
		return PingInfo{}, fmt.Errorf("send daemon handshake: %w", err)
	}
	payload, err := readMessage(conn)
	if err != nil {
		return PingInfo{}, fmt.Errorf("read daemon handshake: %w", err)
	}
	var resp PingResponse
	if err := resp.Decode(payload); err != nil {
		return PingInfo{}, err
	}
	if resp.Error != "" {
		return PingInfo{}, fmt.Errorf("daemon handshake: %s", resp.Error)
	}
	if !resp.Info.OK {
		return PingInfo{}, errors.New("daemon ping returned ok=false")
	}
	if proof != nil {
		if len(resp.Proof) != sha256.Size {
			return PingInfo{}, errors.New("daemon proof response is missing or malformed")
		}
		want, err := proofMAC(proof.key, nonce, ep, resp.Info)
		if err != nil {
			return PingInfo{}, err
		}
		if !hmac.Equal(resp.Proof, want) {
			return PingInfo{}, errors.New("daemon proof mismatch")
		}
	}
	return resp.Info, nil
}

type wireMessage interface {
	Encode() ([]byte, error)
}

type wireDecoder interface {
	Decode([]byte) error
}

// writeMessage frames msg with a 4-byte big-endian length and writes it.
func writeMessage(conn net.Conn, msg wireMessage) error {
	body, err := msg.Encode()
	if err != nil {
		return err
	}
	if len(body) == 0 || len(body) > maxMessageSize {
		return fmt.Errorf("daemon message size %d is invalid", len(body))
	}
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(body)))
	copy(frame[4:], body)
	if _, err := conn.Write(frame); err != nil {
		return fmt.Errorf("write daemon message: %w", err)
	}
	return nil
}

// readMessage reads one length-prefixed message body.
func readMessage(conn net.Conn) ([]byte, error) {
	var size [4]byte
	if _, err := io.ReadFull(conn, size[:]); err != nil {
		return nil, fmt.Errorf("read daemon message size: %w", err)
	}
	length := binary.BigEndian.Uint32(size[:])
	if length == 0 || length > maxMessageSize {
		return nil, fmt.Errorf("daemon message size %d is invalid", length)
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, fmt.Errorf("read daemon message body: %w", err)
	}
	return body, nil
}
