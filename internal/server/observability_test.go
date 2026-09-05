package server

import (
	"bytes"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"yore/internal/wire"
)

// nowFixed is an arbitrary fixed instant: the probe cache is driven by the time
// passed in, so the tests never read the clock.
var nowFixed = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

// captureLog redirects the standard logger into a buffer for the duration of
// the test. The server logs the cause of a 500 there and nowhere else, so the
// buffer is the only place a test can prove the cause was recorded.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(os.Stderr)
		log.SetFlags(flags)
	})
	return &buf
}

// TestWriteAPIErr covers the split that made a storage failure undiagnosable: an
// *apiError is the client's answer and says all it needs to, while any other
// error is answered generically — and so must be logged, or it exists nowhere.
func TestWriteAPIErr(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantBody   string
		wantLogged string // "" => nothing may be logged
	}{
		{
			name:       "apiError keeps its status and message",
			err:        fail(http.StatusBadRequest, "key_id required"),
			wantStatus: http.StatusBadRequest,
			wantBody:   "key_id required",
		},
		{
			name:       "wrapped apiError still maps",
			err:        errors.Join(errors.New("context"), fail(http.StatusConflict, "already claimed")),
			wantStatus: http.StatusConflict,
			wantBody:   "already claimed",
		},
		{
			name:       "storage failure is generic to the client but logged",
			err:        errors.New("write meta: no space left on device"),
			wantStatus: http.StatusInternalServerError,
			wantBody:   "internal error",
			wantLogged: "no space left on device",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLog(t)
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/v1/keys/dek", http.NoBody)

			writeAPIErr(w, r, tc.err)

			require.Equal(t, tc.wantStatus, w.Code, "status")
			require.Contains(t, w.Body.String(), tc.wantBody, "body")
			if tc.wantLogged == "" {
				require.Empty(t, buf.String(), "nothing should be logged")
				return
			}
			require.Contains(t, buf.String(), tc.wantLogged, "cause logged")
			require.Contains(t, buf.String(), "/v1/keys/dek", "path logged")
		})
	}
}

// TestInternalErrorNamesItsCause is the regression the reported outage needed:
// a 500 out of a real request used to leave the access log holding nothing but
// "500", so the reason a write failed could not be recovered from either end.
func TestInternalErrorNamesItsCause(t *testing.T) {
	s, err := New(Options{DBPath: filepath.Join(t.TempDir(), "sync.db"), Token: "tok"})
	require.NoError(t, err, "New")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = s.Close()
	})
	base := &testClient{t: t, base: srv.URL, token: "tok"}
	dc := bootstrapActive(t, base, "dev-1")

	// Storage that answers nothing at all: every transaction fails, which is what
	// a full or broken volume looks like from inside a handler.
	require.NoError(t, s.Close(), "close storage")

	buf := captureLog(t)
	status, body := dc.do("POST", "/v1/keys/dek", []wire.DEKWrap{{
		KeyID: "k1", DeviceID: "dev-1", Epoch: 1, Blob: []byte("blob"), HKVersion: 1,
	}})

	require.Equal(t, http.StatusInternalServerError, status, "status: body %s", body)
	require.Contains(t, string(body), "internal error", "client is told nothing specific")
	require.Contains(t, buf.String(), "internal error:", "server logged the failure")
	require.Contains(t, buf.String(), "/v1/keys/dek", "server logged the endpoint")
}

// TestTempsToPrune covers the pure selector for abandoned snapshot temp files.
// A finished backup must never be selected: those are the copies being kept.
func TestTempsToPrune(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		want  []string
	}{
		{"no temps", []string{"data-1.db", "data-2.db"}, nil},
		{"temps only", []string{".tmp-1.db", ".tmp-2.db"}, []string{".tmp-1.db", ".tmp-2.db"}},
		{"temps beside backups", []string{"data-1.db", ".tmp-9.db"}, []string{".tmp-9.db"}},
		{"unrelated dotfiles left alone", []string{".keep", ".tmp-1.txt", ".tmp-1.db"}, []string{".tmp-1.db"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, tempsToPrune(tc.names))
		})
	}
}

// TestPruneBackupsRemovesTemps proves the collection actually happens on disk: a
// temp file left by a process killed mid-snapshot is a full-size copy of the db
// that nothing else ever removes, so it accumulates on every restart.
func TestPruneBackupsRemovesTemps(t *testing.T) {
	dir := t.TempDir()
	files := []string{"data-1.db", "data-2.db", "data-3.db", ".tmp-111.db", ".tmp-222.db", "notes.txt"}
	for _, name := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600), "seed "+name)
	}

	require.NoError(t, pruneBackups(dir, 2), "prune")

	left := map[string]bool{}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "readdir")
	for _, e := range entries {
		left[e.Name()] = true
	}
	require.Equal(t, map[string]bool{"data-2.db": true, "data-3.db": true, "notes.txt": true}, left,
		"newest two backups and unrelated files survive; temps and older backups do not")
}

// TestReadySeesUnwritableStorage locks in the split between the two open
// endpoints. Reads are served from bbolt's mmap and keep succeeding when the
// volume can no longer take a write, which is exactly how a server that has
// stopped recording history goes on looking healthy.
func TestReadySeesUnwritableStorage(t *testing.T) {
	tests := []struct {
		name       string
		breakDB    bool
		wantReady  int
		wantHealth int
	}{
		{"writable storage is ready", false, http.StatusOK, http.StatusOK},
		{"unwritable storage is not ready, but is still live", true, http.StatusServiceUnavailable, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, err := New(Options{DBPath: filepath.Join(t.TempDir(), "sync.db"), Token: "tok"})
			require.NoError(t, err, "New")
			srv := httptest.NewServer(s.Handler())
			t.Cleanup(func() {
				srv.Close()
				_ = s.Close()
			})
			if tc.breakDB {
				require.NoError(t, s.Close(), "close storage")
			}
			// Open endpoints: an unenrolled caller with no token reaches both.
			c := &testClient{t: t, base: srv.URL}

			status, body := c.do("GET", "/v1/ready", nil)
			require.Equalf(t, tc.wantReady, status, "ready: body %s", body)

			status, body = c.do("GET", "/v1/health", nil)
			require.Equalf(t, tc.wantHealth, status, "health: body %s", body)
		})
	}
}

// TestReadyProbeIsCached keeps the open endpoint from becoming a write
// amplifier: the verdict is reused for readyProbeTTL, so repeated calls cost one
// transaction, not one each.
func TestReadyProbeIsCached(t *testing.T) {
	s, err := New(Options{DBPath: filepath.Join(t.TempDir(), "sync.db"), Token: "tok"})
	require.NoError(t, err, "New")
	t.Cleanup(func() { _ = s.Close() })

	require.True(t, s.readyAt.IsZero(), "a fresh server has not probed yet")
	require.NoError(t, s.probeStorage(nowFixed), "first probe")
	first := s.readyAt

	// Closing storage would fail a real probe; a cached verdict does not run one.
	require.NoError(t, s.Close(), "close storage")
	require.NoError(t, s.probeStorage(nowFixed.Add(readyProbeTTL/2)), "probe within the TTL is cached")
	require.Equal(t, first, s.readyAt, "cached probe does not restamp")

	require.Error(t, s.probeStorage(nowFixed.Add(readyProbeTTL)), "probe past the TTL runs for real")
}
