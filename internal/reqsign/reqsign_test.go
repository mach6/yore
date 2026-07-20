package reqsign

import (
	"crypto/ed25519"
	"net/http"
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	sign := func(b []byte) []byte { return ed25519.Sign(priv, b) }
	body := []byte(`{"host_id":"h","records":[]}`)
	now := time.Unix(1_700_000_000, 0)

	hdrs, err := Sign("dev-1", sign, http.MethodPost, "/v1/records?x=1", body, now)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	for k, v := range hdrs {
		h.Set(k, v)
	}
	if Device(h) != "dev-1" {
		t.Errorf("device = %q", Device(h))
	}
	if err := Verify(h, http.MethodPost, "/v1/records?x=1", body, pub, now); err != nil {
		t.Fatalf("verify: %v", err)
	}
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

	// Tampered body.
	if err := Verify(mk(), "POST", "/v1/x", []byte("other"), pub, now); err == nil {
		t.Error("tampered body accepted")
	}
	// Tampered target (path/method binding).
	if err := Verify(mk(), "POST", "/v1/y", body, pub, now); err == nil {
		t.Error("tampered target accepted")
	}
	if err := Verify(mk(), "DELETE", "/v1/x", body, pub, now); err == nil {
		t.Error("tampered method accepted")
	}
	// Wrong key.
	otherPub, _, _ := ed25519.GenerateKey(nil)
	if err := Verify(mk(), "POST", "/v1/x", body, otherPub, now); err == nil {
		t.Error("wrong key accepted")
	}
	// Stale timestamp (outside skew).
	if err := Verify(mk(), "POST", "/v1/x", body, pub, now.Add(2*Skew)); err == nil {
		t.Error("stale timestamp accepted")
	}
	// Missing headers.
	if err := Verify(http.Header{}, "POST", "/v1/x", body, pub, now); err == nil {
		t.Error("missing headers accepted")
	}
}
