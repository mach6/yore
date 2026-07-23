package server

import (
	"crypto/ed25519"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"yore/internal/wire"
)

// TestBootstrapTokenOnlyFormsAnEmptyGroup is the core of the new enrollment
// model: the server's configured token is the FIRST credential and nothing
// more. It enrolls a device only while the group has none, so it cannot be
// reused later to add machines.
func TestBootstrapTokenOnlyFormsAnEmptyGroup(t *testing.T) {
	c := setup(t)
	dcA := bootstrapActive(t, c, "A")
	_ = dcA

	// The very same token is now refused: an active device exists.
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err, "genkey")
	intruder := c.withKey("B", priv).withToken("tok")
	status, _ := intruder.do("POST", "/v1/devices",
		wire.RegisterReq{ID: "B", Name: "B", PubKey: pubKey(), SignKey: pub})
	assert.Equal(t, http.StatusUnauthorized, status,
		"the bootstrap token must not enroll a second device")
}

// TestTokenIsSingleUse pins redemption: one token admits exactly one machine.
func TestTokenIsSingleUse(t *testing.T) {
	c := setup(t)
	dcA := bootstrapActive(t, c, "A")

	token := mintToken(t, dcA)
	_ = registerWithToken(t, c, "B", token)

	// A second device presenting the same token is refused.
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err, "genkey")
	second := c.withKey("C", priv).withToken(token)
	status, _ := second.do("POST", "/v1/devices",
		wire.RegisterReq{ID: "C", Name: "C", PubKey: pubKey(), SignKey: pub})
	assert.Equal(t, http.StatusUnauthorized, status, "a redeemed token must not enroll again")
}

func TestTokenRequiresAnEnrolledDevice(t *testing.T) {
	c := setup(t)
	_ = bootstrapActive(t, c, "A")

	// An unenrolled caller cannot mint tokens, so enrollment is a closed loop.
	status, _ := c.anon().do("POST", "/v1/tokens", struct{}{})
	assert.Equal(t, http.StatusUnauthorized, status, "minting must require an enrolled device")
}

func TestUnknownTokenIsRefused(t *testing.T) {
	c := setup(t)
	_ = bootstrapActive(t, c, "A")

	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err, "genkey")
	status, _ := c.withKey("B", priv).withToken("not-a-real-token").do("POST", "/v1/devices",
		wire.RegisterReq{ID: "B", Name: "B", PubKey: pubKey(), SignKey: pub})
	assert.Equal(t, http.StatusUnauthorized, status, "an unminted token must be refused")
}

// ---- recovery ----

// initRecovery publishes recovery material signed by the given device.
func initRecovery(t *testing.T, dc *testClient, recPub, recSign []byte) (status int, body []byte) {
	t.Helper()
	return dc.do("POST", "/v1/recovery", wire.RecoveryInit{
		Salt:    []byte("0123456789abcdef"),
		PubKey:  recPub,
		SignKey: recSign,
		Wrap:    wire.HKWrap{DeviceID: "recovery", Blob: []byte("wrapped-hk"), HKVersion: 1},
	})
}

// TestRecoveryFlow covers the whole escape hatch: the salt is readable by
// anyone (it must be, to derive the key at all), but the wrap itself is handed
// over only to a caller that can sign with the recovery key.
func TestRecoveryFlow(t *testing.T) {
	c := setup(t)
	recSign, recSignPriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err, "recovery keygen")

	// Published at bootstrap, by the forming device before it activates.
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err, "genkey")
	dcA := c.withKey("A", priv).withToken("tok")
	status, body := dcA.do("POST", "/v1/devices",
		wire.RegisterReq{ID: "A", Name: "A", PubKey: pubKey(), SignKey: pub})
	require.Equalf(t, http.StatusOK, status, "register A: %s", body)

	status, body = initRecovery(t, dcA, make([]byte, 32), recSign)
	require.Equalf(t, http.StatusOK, status, "init recovery: %s", body)
	activateDevice(t, dcA, "A")

	// The salt is public: recovery is impossible without it.
	status, body = c.anon().do("GET", "/v1/recovery/salt", nil)
	require.Equalf(t, http.StatusOK, status, "salt: %s", body)
	assert.NotEmpty(t, mustJSON[wire.RecoverySalt](t, body).Salt, "salt must be returned")

	// The wrap is not: an unsigned caller gets nothing.
	status, _ = c.anon().do("GET", "/v1/recovery", nil)
	assert.Equal(t, http.StatusUnauthorized, status, "unsigned recovery read must be refused")

	// Signing with the recovery key yields the wrap.
	rec := c.withKey("recovery", recSignPriv)
	status, body = rec.do("GET", "/v1/recovery", nil)
	require.Equalf(t, http.StatusOK, status, "recovery wrap: %s", body)
	assert.Equal(t, []byte("wrapped-hk"), mustJSON[wire.HKWrap](t, body).Blob)

	// A different key must not.
	_, wrongPriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err, "wrong keygen")
	status, _ = c.withKey("recovery", wrongPriv).do("GET", "/v1/recovery", nil)
	assert.Equal(t, http.StatusUnauthorized, status, "a wrong recovery key must be refused")
}

// TestRecoveryIsWriteOnce stops a compromised device from swapping in a
// recovery key of its own.
func TestRecoveryIsWriteOnce(t *testing.T) {
	c := setup(t)
	dcA := bootstrapActive(t, c, "A")

	recSign, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err, "keygen")
	status, body := initRecovery(t, dcA, make([]byte, 32), recSign)
	require.Equalf(t, http.StatusOK, status, "first init: %s", body)

	other, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err, "keygen")
	status, _ = initRecovery(t, dcA, make([]byte, 32), other)
	assert.Equal(t, http.StatusConflict, status, "recovery material must not be replaceable")
}

func TestRecoverySaltAbsentWithoutInit(t *testing.T) {
	c := setup(t)
	_ = bootstrapActive(t, c, "A")

	status, _ := c.anon().do("GET", "/v1/recovery/salt", nil)
	assert.Equal(t, http.StatusUnauthorized, status,
		"with no recovery material there is no tenant to route to")
}
