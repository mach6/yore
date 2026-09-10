package proto

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mach6/yore/internal/rec"
)

// roundtrip writes v with WriteMsg and reads it back into a fresh out with
// ReadMsg over the same buffer.
func roundtrip[T any](t *testing.T, v T, out *T) {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, WriteMsg(&buf, v), "WriteMsg")
	require.NoError(t, ReadMsg(bufio.NewReader(&buf), out), "ReadMsg")
}

func TestRequestRoundTrip(t *testing.T) {
	req := Request{
		Op:     OpQuery,
		Record: &rec.Record{ID: "r1", Cmd: "ls", Exit: rec.IntPtr(0)},
		Query:  &QueryReq{Q: "ls", Scope: ScopeAll, Fuzzy: true, Limit: 50},
	}
	var got Request
	roundtrip(t, req, &got)
	require.Equal(t, req, got, "Request must survive the NDJSON round trip")
}

func TestResponseRoundTrip(t *testing.T) {
	resp := Response{
		OK: true,
		Query: &QueryResp{
			Rows:   []rec.Record{{ID: "r1", Cmd: "ls"}},
			Total:  1,
			Scope:  ScopeLocal,
			Remote: RemoteInfo{State: RemoteOK, Hosts: 2},
		},
		Token: &TokenInfo{Token: "tkt", ExpiresMs: 123},
	}
	var got Response
	roundtrip(t, resp, &got)
	require.Equal(t, resp, got, "Response must survive the round trip")
}

// TestWriteMsgIsOneNewlineTerminatedLine pins the framing contract: exactly one
// line, terminated by a single newline, with no embedded newline.
func TestWriteMsgFraming(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteMsg(&buf, Request{Op: OpPing}))
	out := buf.String()
	require.True(t, strings.HasSuffix(out, "\n"), "message must end in a newline")
	require.Equal(t, 1, strings.Count(out, "\n"), "message must be exactly one line")
}

func TestReadMsgStreamsMultiple(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, WriteMsg(&buf, Request{Op: OpPing}))
	require.NoError(t, WriteMsg(&buf, Request{Op: OpStatus}))

	r := bufio.NewReader(&buf)
	var a, b Request
	require.NoError(t, ReadMsg(r, &a))
	require.NoError(t, ReadMsg(r, &b))
	assert.Equal(t, OpPing, a.Op, "first message")
	assert.Equal(t, OpStatus, b.Op, "second message in order")
}

func TestReadMsgErrors(t *testing.T) {
	t.Run("garbage is an error, not a panic", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader("{not json}\n"))
		var req Request
		require.Error(t, ReadMsg(r, &req))
	})
	t.Run("EOF with no newline is an error", func(t *testing.T) {
		r := bufio.NewReader(strings.NewReader(""))
		var req Request
		require.Error(t, ReadMsg(r, &req))
	})
}

// FuzzReadMsg ensures the socket reader never panics on arbitrary bytes: the
// daemon reads this straight off a client connection.
func FuzzReadMsg(f *testing.F) {
	f.Add([]byte("{\"op\":\"ping\"}\n"))
	f.Add([]byte("garbage\n"))
	f.Add([]byte(""))
	f.Fuzz(func(_ *testing.T, data []byte) {
		var req Request
		_ = ReadMsg(bufio.NewReader(bytes.NewReader(data)), &req)
	})
}
