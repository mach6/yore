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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"yore/internal/wire"
)

// requestTimeout bounds every HTTP call the client makes.
const requestTimeout = 30 * time.Second

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

// HTTPClient is a thin transport over the sync server's HTTP JSON API. It is
// safe for concurrent use: it holds no mutable state beyond the shared
// *http.Client.
type HTTPClient struct {
	baseURL string
	token   string
	hc      *http.Client
}

// NewHTTPClient returns a client for the server at baseURL authenticating with
// the given bearer token. baseURL should have no trailing slash (e.g.
// "https://sync.example.com").
func NewHTTPClient(baseURL, token string) *HTTPClient {
	return &HTTPClient{
		baseURL: baseURL,
		token:   token,
		hc:      &http.Client{Timeout: requestTimeout},
	}
}

// do performs one request. It marshals body (if non-nil) as JSON, sends the
// bearer token unless auth is false, and on a 2xx decodes the response into out
// (if non-nil). A non-2xx becomes an *APIError carrying the server's message.
func (c *HTTPClient) do(ctx context.Context, method, path string, query url.Values, body, out any, auth bool) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("syncer: marshal request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reqBody)
	if err != nil {
		return fmt.Errorf("syncer: build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("syncer: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

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
	return c.do(ctx, http.MethodGet, "/v1/health", nil, nil, nil, false)
}

// Hosts lists every host stream the server knows.
func (c *HTTPClient) Hosts(ctx context.Context) ([]wire.HostInfo, error) {
	var resp wire.HostsResp
	if err := c.do(ctx, http.MethodGet, "/v1/hosts", nil, nil, &resp, true); err != nil {
		return nil, err
	}
	return resp.Hosts, nil
}

// PushRecords uploads a batch of sealed records for one host stream.
func (c *HTTPClient) PushRecords(ctx context.Context, req wire.PushReq) (wire.PushResp, error) {
	var resp wire.PushResp
	err := c.do(ctx, http.MethodPost, "/v1/records", nil, req, &resp, true)
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
	err := c.do(ctx, http.MethodGet, "/v1/records", q, nil, &resp, true)
	return resp, err
}

// RegisterDevice enrolls a new pending device and returns the server's record.
func (c *HTTPClient) RegisterDevice(ctx context.Context, req wire.RegisterReq) (wire.Device, error) {
	var dev wire.Device
	err := c.do(ctx, http.MethodPost, "/v1/devices", nil, req, &dev, true)
	return dev, err
}

// ListDevices returns all devices the server knows, in ID order.
func (c *HTTPClient) ListDevices(ctx context.Context) ([]wire.Device, error) {
	var devs []wire.Device
	if err := c.do(ctx, http.MethodGet, "/v1/devices", nil, nil, &devs, true); err != nil {
		return nil, err
	}
	return devs, nil
}

// ActivateDevice approves a pending device by uploading the HK wrapped for it.
func (c *HTTPClient) ActivateDevice(ctx context.Context, id string, req wire.ActivateReq) error {
	return c.do(ctx, http.MethodPost, "/v1/devices/"+url.PathEscape(id)+"/activate", nil, req, nil, true)
}

// RevokeDevice revokes a device and deletes its HK wrap server-side.
func (c *HTTPClient) RevokeDevice(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/v1/devices/"+url.PathEscape(id)+"/revoke", nil, nil, nil, true)
}

// GetHKWrap fetches the HK wrap sealed to deviceID. The bool is false (with a
// nil error) when the server has no wrap for the device (HTTP 404) — the normal
// state for a pending or revoked device.
func (c *HTTPClient) GetHKWrap(ctx context.Context, deviceID string) (wire.HKWrap, bool, error) {
	q := url.Values{}
	q.Set("device_id", deviceID)
	var wrap wire.HKWrap
	err := c.do(ctx, http.MethodGet, "/v1/keys/hk", q, nil, &wrap, true)
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
	err := c.do(ctx, http.MethodGet, "/v1/keys/dek", q, nil, &resp, true)
	return resp, err
}

// UploadDEKWraps stores DEK wraps and returns how many were newly stored
// (duplicates by KeyID are skipped server-side).
func (c *HTTPClient) UploadDEKWraps(ctx context.Context, wraps []wire.DEKWrap) (int, error) {
	var resp struct {
		Stored int `json:"stored"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/keys/dek", nil, wraps, &resp, true)
	return resp.Stored, err
}

// Rotate atomically installs a new HK generation: a fresh HK wrap set for the
// surviving devices and every DEK re-wrapped under the new HK.
func (c *HTTPClient) Rotate(ctx context.Context, req wire.RotateReq) error {
	return c.do(ctx, http.MethodPost, "/v1/keys/rotate", nil, req, nil, true)
}
