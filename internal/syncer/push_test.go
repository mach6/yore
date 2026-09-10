package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mach6/yore/internal/rec"
	"github.com/mach6/yore/internal/wire"
)

// fatRecords builds n records each carrying roughly bytes of command text, so a
// batch of them exceeds a body limit that a record count alone would not catch.
func fatRecords(n, bytes int) []rec.Record {
	base := int64(1_700_000_000_000)
	out := make([]rec.Record, n)
	for i := range out {
		out[i] = rec.Record{
			ID:      rec.NewID(),
			Cmd:     strings.Repeat("x", bytes),
			Cwd:     "/w",
			StartMs: base + int64(i),
		}
	}
	return out
}

// TestPushSplitsOversizedBatchByBytes is the wedge this change exists to
// prevent. A thousand records is within the server's record cap but far over its
// 10 MiB body cap; before size-aware batching, the push failed, the watermark
// never advanced, and every retry sent the same impossible body forever.
func TestPushSplitsOversizedBatchByBytes(t *testing.T) {
	ctx := context.Background()
	url := newServer(t)
	a, aStore, b, _ := enrollPair(t, url)

	// ~24 KiB each: a thousand of these is ~24 MiB, well past the server's cap,
	// but only 1000 records; the old count-only bound would have sent them all.
	const n, size = 1000, 24 * 1024
	recs := fatRecords(n, size)
	_, err := aStore.AppendBatch(recs)
	require.NoError(t, err, "AppendBatch")

	pushed, err := a.Push(ctx)
	require.NoError(t, err, "a push of large records must succeed by splitting, not fail")
	require.Equal(t, n, pushed, "every record must be uploaded")

	// And the far side actually gets them all, intact.
	got, _, err := b.PullOthers(ctx, map[string]uint64{})
	require.NoError(t, err, "B.PullOthers")
	require.Len(t, got, n)
	byID := make(map[string]rec.Record, len(got))
	for _, r := range got {
		byID[r.ID] = r
	}
	for _, want := range recs {
		require.Equal(t, want.Cmd, byID[want.ID].Cmd, "record %s round-trips intact", want.ID)
	}
}

// TestPushResumesAfterPartialBatch: the watermark tracks exactly what the server
// took, so a second push sends the remainder and nothing twice.
func TestPushResumesAfterPartialBatch(t *testing.T) {
	ctx := context.Background()
	url := newServer(t)
	a, aStore, _, _ := enrollPair(t, url)

	_, err := aStore.AppendBatch(fatRecords(300, 32*1024))
	require.NoError(t, err)

	first, err := a.Push(ctx)
	require.NoError(t, err)
	require.Equal(t, 300, first, "Push loops until the stream is drained")

	second, err := a.Push(ctx)
	require.NoError(t, err)
	assert.Zero(t, second, "a re-push uploads nothing: the watermark persisted")
}

// TestPushRecordSizeTracksBlob sanity-checks the estimate the batching relies
// on: it must grow with the blob and account for base64 expansion.
func TestPushRecordSizeTracksBlob(t *testing.T) {
	small := pushRecordSize(wirePush("id", "key", 100))
	large := pushRecordSize(wirePush("id", "key", 1000))
	assert.Greater(t, large, small)
	assert.GreaterOrEqual(t, large-small, 1200, "base64 expands by 4/3, so 900 more bytes is >=1200 encoded")
}

// TestSyncPromptsOff keeps prompt records on the machine that recorded them
// while the commands they caused still sync.
func TestSyncPromptsOff(t *testing.T) {
	ctx := context.Background()
	url := newServer(t)
	a, aStore, b, _ := enrollPair(t, url)
	a.SetSyncPrompts(false)

	prompt := rec.Record{ID: rec.NewID(), Type: rec.TypePrompt, Prompt: "secret plan", Executor: "claude-code", StartMs: 1_700_000_000_000}
	cmd := rec.Record{ID: rec.NewID(), Cmd: "make", PromptID: prompt.ID, Executor: "claude-code", StartMs: 1_700_000_000_001}
	_, err := aStore.AppendBatch([]rec.Record{prompt, cmd})
	require.NoError(t, err)

	pushed, err := a.Push(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, pushed, "only the command goes up")

	got, _, err := b.PullOthers(ctx, map[string]uint64{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "make", got[0].Cmd)
	assert.Equal(t, prompt.ID, got[0].PromptID, "the command still names its prompt")
	assert.Empty(t, got[0].Prompt, "but the text never left the machine")

	// The watermark passed the skipped prompt: a second push is a no-op, it does
	// not retry the record it deliberately dropped.
	again, err := a.Push(ctx)
	require.NoError(t, err)
	assert.Zero(t, again)
}

// TestSyncPromptsOnByDefault: the switch defaults to syncing, so prompts reach
// other machines unless the user opts out.
func TestSyncPromptsOnByDefault(t *testing.T) {
	ctx := context.Background()
	url := newServer(t)
	a, aStore, b, _ := enrollPair(t, url)

	prompt := rec.Record{ID: rec.NewID(), Type: rec.TypePrompt, Prompt: "shared plan", StartMs: 1_700_000_000_000}
	_, err := aStore.AppendBatch([]rec.Record{prompt})
	require.NoError(t, err)
	_, err = a.Push(ctx)
	require.NoError(t, err)

	got, _, err := b.PullOthers(ctx, map[string]uint64{})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, rec.TypePrompt, got[0].Type)
	assert.Equal(t, "shared plan", got[0].Prompt)
}

// TestPullCiphertextThenOpen is the split the on-disk cache depends on: fetching
// and decrypting are separable, and the two together equal PullOthers.
func TestPullCiphertextThenOpen(t *testing.T) {
	ctx := context.Background()
	url := newServer(t)
	a, aStore, b, _ := enrollPair(t, url)

	_, err := aStore.AppendBatch(makeRecords(50))
	require.NoError(t, err)
	_, err = a.Push(ctx)
	require.NoError(t, err)

	byHost, cursors, err := b.PullCiphertext(ctx, map[string]uint64{})
	require.NoError(t, err, "PullCiphertext")
	require.Len(t, byHost, 1, "one remote host")
	for hostID, sealed := range byHost {
		require.Len(t, sealed, 50)
		assert.Equal(t, sealed[len(sealed)-1].Seq, cursors[hostID], "cursor tracks the last record fetched")
		for _, pr := range sealed {
			assert.NotEmpty(t, pr.Blob, "PullCiphertext returns sealed blobs, never plaintext")
		}

		opened, oerr := b.OpenRecords(ctx, hostID, sealed)
		require.NoError(t, oerr, "OpenRecords")
		require.Len(t, opened, 50)
		assert.NotEmpty(t, opened[0].Cmd, "decryption yields the real command")
	}

	// A second call from the advanced cursors fetches nothing.
	again, _, err := b.PullCiphertext(ctx, cursors)
	require.NoError(t, err)
	for _, sealed := range again {
		assert.Empty(t, sealed, "an advanced cursor must not re-fetch")
	}
}

// TestOpenRecordsRejectsTamperedBlob: decryption failure is loud, never skipped.
func TestOpenRecordsRejectsTamperedBlob(t *testing.T) {
	ctx := context.Background()
	url := newServer(t)
	a, aStore, b, _ := enrollPair(t, url)

	_, err := aStore.AppendBatch(makeRecords(3))
	require.NoError(t, err)
	_, err = a.Push(ctx)
	require.NoError(t, err)

	byHost, _, err := b.PullCiphertext(ctx, map[string]uint64{})
	require.NoError(t, err)
	for hostID, sealed := range byHost {
		require.NotEmpty(t, sealed)
		sealed[0].Blob[len(sealed[0].Blob)-1] ^= 0xff
		_, oerr := b.OpenRecords(ctx, hostID, sealed)
		require.Error(t, oerr, "a tampered blob must abort the pull, not be skipped")
	}
}

func wirePush(id, key string, blobLen int) wire.PushRecord {
	return wire.PushRecord{ID: id, KeyID: key, Blob: make([]byte, blobLen)}
}

// stubPush stands in for the sync server so the backoff can be driven directly:
// it accepts a push of at most accept records and rejects anything larger with
// status. Signatures are not checked: this exercises batch splitting, which the
// real integration tests cannot, since the size estimate deliberately keeps
// bodies under the real server's limit.
func stubPush(t *testing.T, accept, status int) (client *HTTPClient, calls *int) {
	t.Helper()
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		var req wire.PushReq
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Records) > accept {
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(wire.ErrorResp{Error: "too big"})
			return
		}
		_ = json.NewEncoder(w).Encode(wire.PushResp{Stored: len(req.Records)})
	}))
	t.Cleanup(srv.Close)
	return NewHTTPClient(srv.URL, ""), &n
}

func TestPushWithBackoff(t *testing.T) {
	records := make([]wire.PushRecord, 16)
	for i := range records {
		records[i] = wirePush(fmt.Sprintf("id-%d", i), "k", 8)
	}

	tests := []struct {
		name     string
		accept   int
		status   int
		wantSent int
		wantErr  bool
	}{
		{
			name: "413 halves until it fits", accept: 4,
			status: http.StatusRequestEntityTooLarge, wantSent: 4,
		},
		{
			// A truncated body decodes as malformed JSON, so a server that has not
			// learned to say 413 says 400. Halving must recover from that too.
			name: "400 halves until it fits", accept: 8,
			status: http.StatusBadRequest, wantSent: 8,
		},
		{
			name: "no split needed", accept: 16,
			status: http.StatusRequestEntityTooLarge, wantSent: 16,
		},
		{
			// Nothing is acceptable, so halving reaches a single record and stops
			// rather than retrying an impossible batch forever.
			name: "terminates when even one record is refused", accept: 0,
			status: http.StatusRequestEntityTooLarge, wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hc, calls := stubPush(t, tt.accept, tt.status)
			s := &Syncer{http: hc, hostID: "h1"}

			sent, err := s.pushWithBackoff(context.Background(), records)
			if tt.wantErr {
				require.Error(t, err)
				assert.Less(t, *calls, 10, "halving must terminate, not spin")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantSent, sent)
		})
	}
}

func TestRetryableSmaller(t *testing.T) {
	assert.True(t, retryableSmaller(&APIError{Status: http.StatusRequestEntityTooLarge}))
	assert.True(t, retryableSmaller(&APIError{Status: http.StatusBadRequest}))
	assert.False(t, retryableSmaller(&APIError{Status: http.StatusUnauthorized}),
		"an auth failure is not fixed by sending less")
	assert.False(t, retryableSmaller(errors.New("connection refused")),
		"a transport error is not a size rejection")
}
