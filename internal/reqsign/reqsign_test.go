package reqsign

import (
	"crypto/ed25519"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	sign := func(b []byte) []byte { return ed25519.Sign(priv, b) }
	body := []byte(`{"host_id":"h","records":[]}`)
	now := time.Unix(1_700_000_000, 0)

	hdrs, err := Sign("dev-1", sign, http.MethodPost, "/v1/records?x=1", body, now)
	require.NoError(t, err)
	h := http.Header{}
	for k, v := range hdrs {
		h.Set(k, v)
	}
	require.Equal(t, "dev-1", Device(h))
	require.NoError(t, Verify(h, http.MethodPost, "/v1/records?x=1", body, pub, now))
}

func TestVerifyRejects(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	sign := func(b []byte) []byte { return ed25519.Sign(priv, b) }
	body := []byte("body")
	now := time.Unix(1_700_000_000, 0)
	mk := func() http.Header {
		hdrs, _ := Sign("d", sign, "POST", "/v1/x", body, now)
		h := http.Header{}
		for k, v := range hdrs {
			h.Set(k, v)
		}
		return h
	}
	otherPub, _, _ := ed25519.GenerateKey(nil)

	tests := []struct {
		name   string
		hdrs   http.Header
		method string
		path   string
		body   []byte
		pub    ed25519.PublicKey
		now    time.Time
	}{
		{"tampered body", mk(), "POST", "/v1/x", []byte("other"), pub, now},
		{"tampered target (path/method binding)", mk(), "POST", "/v1/y", body, pub, now},
		{"tampered method", mk(), "DELETE", "/v1/x", body, pub, now},
		{"wrong key", mk(), "POST", "/v1/x", body, otherPub, now},
		{"stale timestamp (outside skew)", mk(), "POST", "/v1/x", body, pub, now.Add(2 * Skew)},
		{"missing headers", http.Header{}, "POST", "/v1/x", body, pub, now},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.hdrs, tc.method, tc.path, tc.body, tc.pub, tc.now)
			require.Error(t, err)
		})
	}
}
