package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.etcd.io/bbolt"

	"yore/internal/wire"
)

// multiTenant builds a Server with the default tenant plus one named tenant
// ("alice"), serves it over httptest, and returns a client for each tenant along
// with the Server and its default DBPath.
func multiTenant(t *testing.T) (def, alice *testClient, s *Server, dbPath string) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "sync.db")
	var err error
	s, err = New(Options{
		DBPath:  dbPath,
		Token:   "default-tok",
		Tenants: map[string]string{"alice": "alice-tok"},
	})
	require.NoError(t, err, "New multi-tenant")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = s.Close()
	})
	def = &testClient{t: t, base: srv.URL, token: "default-tok"}
	alice = &testClient{t: t, base: srv.URL, token: "alice-tok"}
	return def, alice, s, dbPath
}

// TestTenantIsolation is the security boundary: one tenant's data must never be
// visible under another tenant's token, and each lives in its own file on disk.
func TestTenantIsolation(t *testing.T) {
	def, alice, _, dbPath := multiTenant(t)

	// Bootstrap a device and push records under the DEFAULT tenant.
	dev := bootstrapActive(t, def, "dev-default")
	status, body := dev.do("POST", "/v1/records", wire.PushReq{HostID: "hostD", Records: mkRecords(1, 3)})
	require.Equalf(t, http.StatusOK, status, "default push: body %s", body)

	// alice sees NO devices, NO hosts, NO records — a completely separate db.
	status, body = alice.do("GET", "/v1/devices", nil)
	require.Equalf(t, http.StatusOK, status, "alice list devices: body %s", body)
	require.Empty(t, mustJSON[[]wire.Device](t, body), "alice must not see default's device")

	status, body = alice.do("GET", "/v1/hosts", nil)
	require.Equalf(t, http.StatusOK, status, "alice hosts: body %s", body)
	require.Empty(t, mustJSON[wire.HostsResp](t, body).Hosts, "alice must not see default's hosts")

	status, body = alice.do("GET", "/v1/records?host_id=hostD", nil)
	require.Equalf(t, http.StatusOK, status, "alice pull: body %s", body)
	require.Empty(t, mustJSON[wire.PullResp](t, body).Records, "alice must not see default's records")

	// The default tenant still sees its own device and records.
	status, body = def.do("GET", "/v1/devices", nil)
	require.Equalf(t, http.StatusOK, status, "default list devices: body %s", body)
	require.Len(t, mustJSON[[]wire.Device](t, body), 1, "default sees its device")

	status, body = def.do("GET", "/v1/records?host_id=hostD", nil)
	require.Equalf(t, http.StatusOK, status, "default pull: body %s", body)
	require.Len(t, mustJSON[wire.PullResp](t, body).Records, 3, "default sees its records")

	// alice can independently bootstrap her OWN device — no collision with the
	// default tenant's id space, since they are different files.
	_ = bootstrapActive(t, alice, "dev-alice")
	status, body = alice.do("GET", "/v1/devices", nil)
	require.Equalf(t, http.StatusOK, status, "alice list after own bootstrap: body %s", body)
	require.Len(t, mustJSON[[]wire.Device](t, body), 1, "alice sees only her own device")
	_, body = def.do("GET", "/v1/devices", nil)
	require.Len(t, mustJSON[[]wire.Device](t, body), 1, "default still sees only its own device")

	// The two tenants are separate files on disk.
	_, err := os.Stat(dbPath)
	require.NoError(t, err, "default db file must exist")
	aliceDB := filepath.Join(filepath.Dir(dbPath), "tenants", "alice.db")
	_, err = os.Stat(aliceDB)
	require.NoErrorf(t, err, "alice db file must exist at %s", aliceDB)
	require.NotEqual(t, dbPath, aliceDB, "tenant files must differ")
}

// TestTenantAuth checks token routing: garbage/absent tokens are rejected, and
// each valid tenant token authenticates to its own tenant only.
func TestTenantAuth(t *testing.T) {
	def, alice, _, _ := multiTenant(t)

	bad := &testClient{t: t, base: def.base, token: "garbage"}
	status, _ := bad.do("GET", "/v1/devices", nil)
	require.Equal(t, http.StatusUnauthorized, status, "unknown token => 401")

	none := &testClient{t: t, base: def.base, token: ""}
	status, _ = none.do("GET", "/v1/devices", nil)
	require.Equal(t, http.StatusUnauthorized, status, "no token => 401")

	status, _ = def.do("GET", "/v1/devices", nil)
	require.Equal(t, http.StatusOK, status, "default token authenticates")
	status, _ = alice.do("GET", "/v1/devices", nil)
	require.Equal(t, http.StatusOK, status, "alice token authenticates")

	// Health stays open with no token even in multi-tenant mode.
	status, _ = none.do("GET", "/v1/health", nil)
	require.Equal(t, http.StatusOK, status, "health open")
}

// TestNewBadTenantName rejects tenant names that are unsafe as filenames or
// collide with the reserved default tenant.
func TestNewBadTenantName(t *testing.T) {
	tests := []struct {
		name    string
		tenants map[string]string
	}{
		{"slash", map[string]string{"a/b": "t"}},
		{"dotdot", map[string]string{"../evil": "t"}},
		{"empty", map[string]string{"": "t"}},
		{"reserved default", map[string]string{"default": "t"}},
		{"space", map[string]string{"a b": "t"}},
		{"dot", map[string]string{"a.b": "t"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New(Options{
				DBPath:  filepath.Join(t.TempDir(), "sync.db"),
				Token:   "tok",
				Tenants: tc.tenants,
			})
			require.Error(t, err, "expected error for tenant name")
		})
	}
}

// TestNewTenantTokenErrors rejects an empty or duplicate tenant token.
func TestNewTenantTokenErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := New(Options{DBPath: filepath.Join(dir, "a.db"), Token: "tok", Tenants: map[string]string{"alice": ""}})
	require.Error(t, err, "empty tenant token must be rejected")

	_, err = New(Options{DBPath: filepath.Join(dir, "b.db"), Token: "dup", Tenants: map[string]string{"alice": "dup"}})
	require.Error(t, err, "a tenant reusing another's token must be rejected")
}

// TestMustDBMissingContext proves mustDB fails closed (500) when no tenant db is
// bound — it must never fall back to a shared db.
func TestMustDBMissingContext(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/devices", http.NoBody)
	db, ok := mustDB(rec, req)
	require.False(t, ok, "mustDB must report failure")
	require.Nil(t, db, "mustDB must return no db")
	require.Equal(t, http.StatusInternalServerError, rec.Code, "mustDB must write 500")
}

// TestHandlerRefusesMissingTenantDB drives a real handler with no tenant db in
// context and asserts it 500s rather than reaching for another tenant's storage.
func TestHandlerRefusesMissingTenantDB(t *testing.T) {
	_, _, s, _ := multiTenant(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/devices", http.NoBody)
	s.handleListDevices(rec, req)
	require.Equal(t, http.StatusInternalServerError, rec.Code, "handler must 500, never fall back")
}

// TestBackupPerTenant snapshots every tenant and confirms each snapshot is a
// valid bbolt db containing only that tenant's data.
func TestBackupPerTenant(t *testing.T) {
	def, alice, s, dbPath := multiTenant(t)

	devD := bootstrapActive(t, def, "dev-d")
	devD.do("POST", "/v1/records", wire.PushReq{HostID: "hd", Records: mkRecords(1, 2)})
	devA := bootstrapActive(t, alice, "dev-a")
	devA.do("POST", "/v1/records", wire.PushReq{HostID: "ha", Records: mkRecords(1, 4)})

	s.backupAll()

	base := filepath.Dir(dbPath)
	cases := []struct {
		tenant    string
		ownHost   string
		ownN      int
		otherHost string
	}{
		{"default", "records:hd", 2, "records:ha"},
		{"alice", "records:ha", 4, "records:hd"},
	}
	for _, tc := range cases {
		t.Run(tc.tenant, func(t *testing.T) {
			dir := filepath.Join(base, "backups", tc.tenant)
			entries, err := os.ReadDir(dir)
			require.NoErrorf(t, err, "read backup dir %s", tc.tenant)
			var snap string
			for _, e := range entries {
				if _, ok := backupTimestamp(e.Name()); ok {
					snap = filepath.Join(dir, e.Name())
				}
			}
			require.NotEmptyf(t, snap, "no snapshot for tenant %s", tc.tenant)

			bdb, err := bbolt.Open(snap, 0o600, &bbolt.Options{Timeout: time.Second, ReadOnly: true})
			require.NoErrorf(t, err, "reopen snapshot %s", tc.tenant)
			require.NoError(t, bdb.View(func(tx *bbolt.Tx) error {
				own := tx.Bucket([]byte(tc.ownHost))
				require.NotNilf(t, own, "snapshot %s missing its own bucket %s", tc.tenant, tc.ownHost)
				n := 0
				require.NoError(t, own.ForEach(func(_, _ []byte) error { n++; return nil }), "count records")
				require.Equalf(t, tc.ownN, n, "snapshot %s record count", tc.tenant)
				require.Nilf(t, tx.Bucket([]byte(tc.otherHost)), "snapshot %s must not contain other tenant's bucket", tc.tenant)
				return nil
			}), "view snapshot")
			require.NoError(t, bdb.Close(), "close snapshot")
		})
	}
}

// TestPruneBackupsKeepsNewest confirms prune keeps the newest N and leaves
// non-backup files alone.
func TestPruneBackupsKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	for _, ts := range []int64{100, 200, 300, 400, 500} {
		name := filepath.Join(dir, fmt.Sprintf("data-%d.db", ts))
		require.NoError(t, os.WriteFile(name, []byte("x"), 0o600), "write backup")
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("keep me"), 0o600), "write stray")

	require.NoError(t, pruneBackups(dir, 2), "prune")

	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "readdir")
	got := map[string]bool{}
	for _, e := range entries {
		got[e.Name()] = true
	}
	require.True(t, got["data-500.db"], "newest kept")
	require.True(t, got["data-400.db"], "2nd newest kept")
	require.False(t, got["data-300.db"], "older pruned")
	require.False(t, got["data-200.db"], "older pruned")
	require.False(t, got["data-100.db"], "oldest pruned")
	require.True(t, got["notes.txt"], "non-backup file untouched")
}

// TestBackupsToPrune covers the pure prune selector.
func TestBackupsToPrune(t *testing.T) {
	tests := []struct {
		name  string
		names []string
		keep  int
		want  []string
	}{
		{"keep all when fewer than keep", []string{"data-1.db", "data-2.db"}, 3, nil},
		{"prune oldest", []string{"data-3.db", "data-1.db", "data-2.db"}, 1, []string{"data-1.db", "data-2.db"}},
		{"ignore non-matching names", []string{"data-2.db", "junk", "data-1.db", ".tmp-9.db"}, 1, []string{"data-1.db"}},
		{"keep zero prunes all matching", []string{"data-2.db", "data-1.db"}, 0, []string{"data-1.db", "data-2.db"}},
		{"negative keep treated as zero", []string{"data-1.db"}, -1, []string{"data-1.db"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, backupsToPrune(tc.names, tc.keep))
		})
	}
}

// TestBackupLoopRuns exercises the backup loop end to end with short timings and
// a done channel, waiting until a snapshot appears before stopping.
func TestBackupLoopRuns(t *testing.T) {
	_, _, s, dbPath := multiTenant(t)
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		s.backupLoopEvery(done, 5*time.Millisecond, 10*time.Millisecond)
		close(exited)
	}()

	defDir := filepath.Join(filepath.Dir(dbPath), "backups", "default")
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(defDir)
		if err != nil {
			return false
		}
		for _, e := range entries {
			if _, ok := backupTimestamp(e.Name()); ok {
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond, "expected a backup snapshot")

	close(done)
	<-exited // ensure the loop stops before cleanup closes the dbs
}
