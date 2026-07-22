package wire

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// roundtrip marshals v and unmarshals it back into out, asserting equality.
func roundtrip[T any](t *testing.T, v T, out *T) {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err, "marshal")
	require.NoError(t, json.Unmarshal(b, out), "unmarshal")
}

func TestDeviceRoundTrip(t *testing.T) {
	d := Device{
		ID:      "01J",
		Name:    "laptop",
		PubKey:  []byte{1, 2, 3, 4},
		SignKey: []byte{5, 6, 7, 8},
		Status:  DeviceActive,
	}
	var got Device
	roundtrip(t, d, &got)
	require.Equal(t, d, got)
}

// TestBlobFieldsAreBase64 pins the wire contract: every []byte field encodes as
// base64, which is how ciphertext crosses the wire.
func TestBlobFieldsAreBase64(t *testing.T) {
	b, err := json.Marshal(HKWrap{DeviceID: "d", Blob: []byte("hello"), HKVersion: 1})
	require.NoError(t, err)
	// base64("hello") = aGVsbG8=
	assert.Contains(t, string(b), "\"blob\":\"aGVsbG8=\"", "[]byte must marshal as base64")
}

func TestPushAndPullRoundTrip(t *testing.T) {
	push := PushReq{
		HostID: "h1",
		Records: []PushRecord{
			{Seq: 1, ID: "r1", KeyID: "k1", Blob: []byte("a")},
			{Seq: 2, ID: "r2", KeyID: "k1", Blob: []byte("b")},
		},
	}
	var gotPush PushReq
	roundtrip(t, push, &gotPush)
	require.Equal(t, push, gotPush)

	next := uint64(49000)
	pull := PullResp{
		Records:   []PullRecord{{Seq: 1, ID: "r1", KeyID: "k1", Blob: []byte("a"), CreatedMs: 5}},
		NextAfter: &next,
	}
	var gotPull PullResp
	roundtrip(t, pull, &gotPull)
	require.Equal(t, pull, gotPull)
	require.NotNil(t, gotPull.NextAfter)
	require.Equal(t, next, *gotPull.NextAfter)
}

// TestNextAfterOmittedWhenCaughtUp pins the paging signal: a nil NextAfter (no
// more rows) must not appear in the JSON, so the client sees "caught up".
func TestNextAfterOmittedWhenCaughtUp(t *testing.T) {
	b, err := json.Marshal(PullResp{Records: []PullRecord{}})
	require.NoError(t, err)
	assert.NotContains(t, string(b), "next_after", "a nil NextAfter must be omitted")
}

func TestEnrollmentTypesRoundTrip(t *testing.T) {
	t.Run("RegisterResp", func(t *testing.T) {
		v := RegisterResp{Device: Device{ID: "x", Status: DevicePending}, GroupFormed: true}
		var got RegisterResp
		roundtrip(t, v, &got)
		require.Equal(t, v, got)
	})
	t.Run("TicketResp", func(t *testing.T) {
		v := TicketResp{Ticket: "tkt", ExpiresMs: 123}
		var got TicketResp
		roundtrip(t, v, &got)
		require.Equal(t, v, got)
	})
	t.Run("RecoveryInit", func(t *testing.T) {
		v := RecoveryInit{
			Salt:    []byte{9, 9},
			PubKey:  []byte{1},
			SignKey: []byte{2},
			Wrap:    HKWrap{DeviceID: "recovery", Blob: []byte{3}, HKVersion: 1},
		}
		var got RecoveryInit
		roundtrip(t, v, &got)
		require.Equal(t, v, got)
	})
	t.Run("RotateReq", func(t *testing.T) {
		v := RotateReq{
			HKVersion: 2,
			HKWraps:   []HKWrap{{DeviceID: "d", Blob: []byte{1}, HKVersion: 2}},
			DEKWraps:  []DEKWrap{{KeyID: "k", DeviceID: "d", Epoch: 5, Blob: []byte{2}, HKVersion: 2}},
		}
		var got RotateReq
		roundtrip(t, v, &got)
		require.Equal(t, v, got)
	})
}

func TestErrorRespRoundTrip(t *testing.T) {
	var got ErrorResp
	roundtrip(t, ErrorResp{Error: "unauthorized"}, &got)
	require.Equal(t, "unauthorized", got.Error)
}

// FuzzDecodeWireTypes ensures decoding arbitrary bytes into the request types
// the server accepts never panics — these decode straight off the network.
func FuzzDecodeWireTypes(f *testing.F) {
	f.Add([]byte(`{"host_id":"h","records":[{"seq":1,"blob":"AA=="}]}`))
	f.Add([]byte(`{"id":"x","pub_key":"AA==","sign_key":"AA=="}`))
	f.Add([]byte(`{`))
	f.Add([]byte(strings.Repeat("[", 100)))
	f.Fuzz(func(_ *testing.T, data []byte) {
		var push PushReq
		_ = json.Unmarshal(data, &push)
		var reg RegisterReq
		_ = json.Unmarshal(data, &reg)
		var rot RotateReq
		_ = json.Unmarshal(data, &rot)
		var ri RecoveryInit
		_ = json.Unmarshal(data, &ri)
	})
}
