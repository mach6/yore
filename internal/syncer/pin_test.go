package syncer

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/mach6/yore/internal/server"
)

// tlsServer starts the sync API behind an httptest TLS server.
func tlsServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv, err := server.New(server.Options{
		DBPath: filepath.Join(t.TempDir(), "sync.db"),
		Token:  testToken,
	})
	require.NoError(t, err, "server.New")
	ts := httptest.NewTLSServer(srv.Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() { _ = srv.Close() })
	return ts
}

// pinnedClient returns a client for ts that pins pin and trusts ts's
// self-signed CA. The trust is the point: pinVerifier runs in ADDITION
// to the standard chain check, so without it every case here would fail on an
// unknown authority and the pin would never be reached.
func pinnedClient(t *testing.T, ts *httptest.Server, pin string) *HTTPClient {
	t.Helper()
	c := NewHTTPClient(ts.URL, pin)
	tr, ok := c.hc.Transport.(*http.Transport)
	require.True(t, ok, "transport should be *http.Transport")
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	pool := x509.NewCertPool()
	pool.AddCert(ts.Certificate())
	tr.TLSClientConfig.RootCAs = pool
	return c
}

// TestPinEnforcement covers the whole pin flow as a user meets it: `yore setup
// --pin` captures the server's SPKI (ServerPin), a later connection presenting
// that same key is accepted, and one presenting any other key is refused.
//
// The mismatch case also pins the plumbing yore doctor depends on: the refusal
// has to survive net/http wrapping the handshake failure in a *url.Error, or
// PinMismatch would never match and doctor would report a generic "unreachable".
func TestPinEnforcement(t *testing.T) {
	ts := tlsServer(t)
	live, err := ServerPin(ts.URL)
	require.NoError(t, err, "ServerPin")
	require.NotEmpty(t, live, "captured pin")

	// A syntactically valid pin for some other key: what a rotated certificate or
	// an intercepting proxy presents.
	other := base64.StdEncoding.EncodeToString(make([]byte, 32))

	tests := []struct {
		name  string
		pin   string
		match bool
	}{
		{name: "no pin connects", pin: "", match: true},
		{name: "captured pin connects", pin: live, match: true},
		{name: "other key is refused", pin: other, match: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := pinnedClient(t, ts, tc.pin).Health(context.Background())
			if tc.match {
				require.NoError(t, err, "Health should connect")
				return
			}
			require.Error(t, err, "Health should refuse the wrong certificate")
			pe, ok := PinMismatch(err)
			require.True(t, ok, "refusal should be a *PinError through the url.Error wrap: %v", err)
			require.Equal(t, tc.pin, pe.Want, "Want should be the configured pin")
			require.Equal(t, live, pe.Got, "Got should be the certificate actually served")
		})
	}
}

// TestPinMismatchOnlyMatchesPinErrors: doctor branches on PinMismatch, so a
// server that is merely down or refusing must not be reported as a bad pin.
func TestPinMismatchOnlyMatchesPinErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "nil", err: nil},
		{name: "api error", err: &APIError{Status: 401, Msg: "unknown device"}},
		{name: "transport error", err: context.DeadlineExceeded},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pe, ok := PinMismatch(tc.err)
			require.False(t, ok, "should not be reported as a pin mismatch")
			require.Nil(t, pe)
		})
	}
}

// TestPinErrorNamesBothDigests: the message is the only thing a user sees when
// the daemon logs a failed sync cycle, so it has to carry what was pinned AND
// what arrived; one digest alone says nothing about what changed.
func TestPinErrorNamesBothDigests(t *testing.T) {
	msg := (&PinError{Want: "AAAApinned", Got: "BBBBserved"}).Error()
	require.Contains(t, msg, "AAAApinned")
	require.Contains(t, msg, "BBBBserved")
}
