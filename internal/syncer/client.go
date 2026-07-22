// Package syncer is yore's cross-machine sync engine. It is the component that
// makes the headline feature real: it encrypts this host's history with the
// cryptobox key hierarchy, pushes only ciphertext to the sync server, pulls
// other machines' ciphertext, and decrypts it locally. The server never sees
// plaintext or any key.
//
// The package has two parts:
//
//   - HTTPClient — a thin, stateless transport over the server's REST API
//     (one method per endpoint, wire types in and out, typed errors).
//   - Syncer — the engine that owns all cryptobox usage. It holds unwrapped
//     keys (the History Key and epoch DEKs) in RAM only, never on disk.
package syncer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"yore/internal/reqsign"
	"yore/internal/wire"
)

// requestTimeout bounds every HTTP call the client makes.
const requestTimeout = 30 * time.Second

// connectTimeout bounds only the TCP connect. A server that is simply down
// refuses immediately, but one that is unreachable (no route, black-holed
// network, VPN off) would otherwise hang for the full requestTimeout — and the
// daemon's sync cycle along with it. Failing the connect fast keeps "the
// network is down" a brief, quiet degradation to local-only history.
const connectTimeout = 5 * time.Second

// APIError is a non-2xx response from the sync server. It carries the HTTP
// status and the server's ErrorResp.Error text.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("yore server: http %d", e.Status)
	}
	return fmt.Sprintf("yore server: http %d: %s", e.Status, e.Msg)
}

// HTTPClient is a thin transport over the sync server's HTTP JSON API.
//
// There is no bearer token: EVERY authenticated request — reads included — is
// signed with the device's Ed25519 key (set once via SetSigner), so the only
// credential is a private key that never leaves the machine. Enrollment, which
// happens before a device record exists, is authorized instead by a single-use
// ticket passed to RegisterDevice.
type HTTPClient struct {
	baseURL  string
	hc       *http.Client
	deviceID string
	sign     func([]byte) []byte // nil until SetSigner
}

// NewHTTPClient returns a client for the server at baseURL. Authentication is
// per-device signatures, installed by SetSigner; there is no token to pass.
// baseURL should have no trailing slash. If pin is
// non-empty (base64 SHA-256 of the server's SubjectPublicKeyInfo), the client
// pins the server's TLS certificate and refuses any other — defeating a
// TLS-inspecting proxy at the cost of not syncing through one.
func NewHTTPClient(baseURL, pin string) *HTTPClient {
	// Mirror http.DefaultTransport's proxy behaviour; only the dial timeout (and
	// optionally the pin) differ from the stdlib default.
	tr := &http.Transport{
		Proxy:       http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{Timeout: connectTimeout}).DialContext,
	}
	if pin != "" {
		tr.TLSClientConfig = &tls.Config{VerifyConnection: pinVerifier(pin)}
	}
	return &HTTPClient{baseURL: baseURL, hc: &http.Client{Timeout: requestTimeout, Transport: tr}}
}

// SetSigner installs the request signer (deviceID + Ed25519 sign function).
// Call once at Syncer construction; it makes every request carry a reqsign
// signature. Recovery reuses this with the recovery-derived key and the
// reserved device id, which is how a machine with no enrolled identity proves
// it holds the passphrase.
func (c *HTTPClient) SetSigner(deviceID string, sign func([]byte) []byte) {
	c.deviceID = deviceID
	c.sign = sign
}

// pinVerifier returns a TLS VerifyConnection callback enforcing that the leaf
// certificate's SPKI SHA-256 equals the pinned value.
func pinVerifier(pin string) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("syncer: no server certificate to pin")
		}
		sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
		got := base64.StdEncoding.EncodeToString(sum[:])
		if got != pin {
			return fmt.Errorf("syncer: server certificate pin mismatch (got %s) — refusing (TLS interception or changed cert?)", got)
		}
		return nil
	}
}

// ServerPin fetches the server's current certificate SPKI pin (base64 SHA-256),
// for `yore setup --pin` to capture. It performs a TLS handshake to baseURL's
// host and does NOT verify the chain (we're capturing whatever cert is present);
// run it on a trusted network so you don't pin an interceptor.
func ServerPin(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	host := u.Host
	if u.Port() == "" {
		host += ":443"
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", host,
		&tls.Config{InsecureSkipVerify: true}) //nolint:gosec // capturing the cert to pin, not trusting it
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	certs := conn.ConnectionState().PeerCertificates
	if len(certs) == 0 {
		return "", errors.New("no server certificate")
	}
	sum := sha256.Sum256(certs[0].RawSubjectPublicKeyInfo)
	return base64.StdEncoding.EncodeToString(sum[:]), nil
}

// do performs one request: it marshals body (if non-nil) as JSON, applies hdrs,
// signs the request, and on a 2xx decodes the response into out (if non-nil).
// A non-2xx becomes an *APIError carrying the server's message.
func (c *HTTPClient) do(ctx context.Context, method, path string, query url.Values, body, out any, hdrs map[string]string) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var bodyBytes []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("syncer: marshal request: %w", err)
		}
		bodyBytes = b
	}

	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(bodyBytes))
	if err != nil {
		return fmt.Errorf("syncer: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdrs {
		req.Header.Set(k, v)
	}
	// Sign every request with the device key: it is the only credential. reqsign
	// binds the signature to method+target+body.
	if c.sign != nil {
		hdrs, err := reqsign.Sign(c.deviceID, c.sign, method, req.URL.RequestURI(), bodyBytes, time.Now())
		if err != nil {
			return fmt.Errorf("syncer: sign request: %w", err)
		}
		for k, v := range hdrs {
			req.Header.Set(k, v)
		}
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("syncer: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("syncer: read response: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &APIError{Status: resp.StatusCode, Msg: parseServerError(data)}
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("syncer: decode response: %w", err)
		}
	}
	return nil
}

// parseServerError pulls ErrorResp.Error from a response body, falling back to
// the raw (trimmed) body when it is not the expected JSON shape.
func parseServerError(data []byte) string {
	var er wire.ErrorResp
	if err := json.Unmarshal(data, &er); err == nil && er.Error != "" {
		return er.Error
	}
	return string(bytes.TrimSpace(data))
}

// Health checks server liveness. It sends no auth token (the endpoint is open).
func (c *HTTPClient) Health(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/v1/health", nil, nil, nil, nil)
}

// Hosts lists every host stream the server knows.
func (c *HTTPClient) Hosts(ctx context.Context) ([]wire.HostInfo, error) {
	var resp wire.HostsResp
	if err := c.do(ctx, http.MethodGet, "/v1/hosts", nil, nil, &resp, nil); err != nil {
		return nil, err
	}
	return resp.Hosts, nil
}

// PushRecords uploads a batch of sealed records for one host stream.
func (c *HTTPClient) PushRecords(ctx context.Context, req wire.PushReq) (wire.PushResp, error) {
	var resp wire.PushResp
	err := c.do(ctx, http.MethodPost, "/v1/records", nil, req, &resp, nil)
	return resp, err
}

// PullRecords pages one host's sealed stream after the given cursor.
func (c *HTTPClient) PullRecords(ctx context.Context, hostID string, after uint64, limit int) (wire.PullResp, error) {
	q := url.Values{}
	q.Set("host_id", hostID)
	q.Set("after", strconv.FormatUint(after, 10))
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var resp wire.PullResp
	err := c.do(ctx, http.MethodGet, "/v1/records", q, nil, &resp, nil)
	return resp, err
}

// RegisterDevice enrolls a new pending device, authorized by a single-use
// enrollment ticket. The ticket rides in a header rather than the body so it is
// never marshalled into anything the server persists.
func (c *HTTPClient) RegisterDevice(ctx context.Context, req wire.RegisterReq, ticket string) (wire.RegisterResp, error) {
	var resp wire.RegisterResp
	err := c.do(ctx, http.MethodPost, "/v1/devices", nil, req, &resp,
		map[string]string{HdrTicket: ticket})
	return resp, err
}

// HdrTicket carries the single-use enrollment ticket (mirrors the server).
const HdrTicket = "X-Yore-Ticket"

// RecoveryDeviceID is the reserved signer id recovery requests use: the caller
// has no enrolled device, and proves itself with the recovery key instead.
const RecoveryDeviceID = "recovery"

// MintTicket asks the server for a fresh single-use enrollment ticket. Only an
// enrolled device can call it, which is what makes enrollment a closed loop.
func (c *HTTPClient) MintTicket(ctx context.Context) (wire.TicketResp, error) {
	var resp wire.TicketResp
	err := c.do(ctx, http.MethodPost, "/v1/tickets", nil, struct{}{}, &resp, nil)
	return resp, err
}

// InitRecovery publishes the recovery public keys and the History Key sealed to
// them. Called once, by the device that forms the group.
func (c *HTTPClient) InitRecovery(ctx context.Context, req wire.RecoveryInit) error {
	return c.do(ctx, http.MethodPost, "/v1/recovery", nil, req, nil, nil)
}

// RecoverySalt fetches the Argon2id salt needed to derive the recovery key.
// Unauthenticated: it is required before any recovery signature is possible.
func (c *HTTPClient) RecoverySalt(ctx context.Context) (wire.RecoverySalt, error) {
	var resp wire.RecoverySalt
	err := c.do(ctx, http.MethodGet, "/v1/recovery/salt", nil, nil, &resp, nil)
	return resp, err
}

// RecoveryTicket mints an enrollment ticket authorized by the recovery key, so
// a replacement machine can enroll when no surviving device can vouch for it.
// The client must already be signing with the recovery key.
func (c *HTTPClient) RecoveryTicket(ctx context.Context) (wire.TicketResp, error) {
	var resp wire.TicketResp
	err := c.do(ctx, http.MethodPost, "/v1/recovery/ticket", nil, struct{}{}, &resp, nil)
	return resp, err
}

// RecoveryActivate admits a device using the recovery key's authority, for when
// no surviving device can approve it.
func (c *HTTPClient) RecoveryActivate(ctx context.Context, id string, req wire.ActivateReq) error {
	return c.do(ctx, http.MethodPost, "/v1/recovery/activate/"+url.PathEscape(id), nil, req, nil, nil)
}

// RecoveryWrap fetches the History Key wrapped to the recovery key. The client
// must already be signing with the recovery key (see SetSigner).
func (c *HTTPClient) RecoveryWrap(ctx context.Context) (wire.HKWrap, error) {
	var wrap wire.HKWrap
	err := c.do(ctx, http.MethodGet, "/v1/recovery", nil, nil, &wrap, nil)
	return wrap, err
}

// ListDevices returns all devices the server knows, in ID order.
func (c *HTTPClient) ListDevices(ctx context.Context) ([]wire.Device, error) {
	var devs []wire.Device
	if err := c.do(ctx, http.MethodGet, "/v1/devices", nil, nil, &devs, nil); err != nil {
		return nil, err
	}
	return devs, nil
}

// ActivateDevice approves a pending device by uploading the HK wrapped for it.
func (c *HTTPClient) ActivateDevice(ctx context.Context, id string, req wire.ActivateReq) error {
	return c.do(ctx, http.MethodPost, "/v1/devices/"+url.PathEscape(id)+"/activate", nil, req, nil, nil)
}

// RevokeDevice revokes a device and deletes its HK wrap server-side.
func (c *HTTPClient) RevokeDevice(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/devices/"+url.PathEscape(id)+"/revoke", nil, nil, nil, nil)
}

// GetHKWrap fetches the HK wrap sealed to deviceID. The bool is false (with a
// nil error) when the server has no wrap for the device (HTTP 404) — the normal
// state for a pending or revoked device.
func (c *HTTPClient) GetHKWrap(ctx context.Context, deviceID string) (wire.HKWrap, bool, error) {
	q := url.Values{}
	q.Set("device_id", deviceID)
	var wrap wire.HKWrap
	err := c.do(ctx, http.MethodGet, "/v1/keys/hk", q, nil, &wrap, nil)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == http.StatusNotFound {
			return wire.HKWrap{}, false, nil
		}
		return wire.HKWrap{}, false, err
	}
	return wrap, true, nil
}

// ListDEKWraps pages the DEK wrap set. cursor is the last KeyID seen ("" from
// the start); the returned NextCursor is "" once caught up.
func (c *HTTPClient) ListDEKWraps(ctx context.Context, cursor string, limit int) (wire.DEKListResp, error) {
	q := url.Values{}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var resp wire.DEKListResp
	err := c.do(ctx, http.MethodGet, "/v1/keys/dek", q, nil, &resp, nil)
	return resp, err
}

// UploadDEKWraps stores DEK wraps and returns how many were newly stored
// (duplicates by KeyID are skipped server-side).
func (c *HTTPClient) UploadDEKWraps(ctx context.Context, wraps []wire.DEKWrap) (int, error) {
	var resp struct {
		Stored int `json:"stored"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/keys/dek", nil, wraps, &resp, nil)
	return resp.Stored, err
}

// Rotate atomically installs a new HK generation: a fresh HK wrap set for the
// surviving devices and every DEK re-wrapped under the new HK.
func (c *HTTPClient) Rotate(ctx context.Context, req wire.RotateReq) error {
	return c.do(ctx, http.MethodPost, "/v1/keys/rotate", nil, req, nil, nil)
}
