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

	"github.com/mach6/yore/internal/wire"
)

// multiTenant builds a Server with two named tenants ("alice", "bob") and no
// single-token tenant, serves it over httptest, and returns a client for each
// tenant along with the Server and the DBPath rooting tenants/ and backups/.
func multiTenant(t *testing.T) (alice, bob *testClient, s *Server, dbPath string) {
	t.Helper()
	dbPath = filepath.Join(t.TempDir(), "sync.db")
	var err error
	s, err = New(Options{
		DBPath:  dbPath,
		Tenants: map[string]string{"alice": "alice-tok", "bob": "bob-tok"},
	})
	require.NoError(t, err, "New multi-tenant")
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		srv.Close()
		_ = s.Close()
	})
	// Each tenant gets a bootstrapped active device: routing is now by device
	// record, so a tenant with no device has nothing to route by.
	alice = bootstrapActive(t, &testClient{t: t, base: srv.URL, token: "alice-tok"}, "alice-root")
	bob = bootstrapActive(t, &testClient{t: t, base: srv.URL, token: "bob-tok"}, "bob-root")
	return alice, bob, s, dbPath
}

// TestNewTokenModes pins the either/or: a single token, or named tenants, never
// both and never neither.
func TestNewTokenModes(t *testing.T) {
	t.Run("neither is refused", func(t *testing.T) {
		_, err := New(Options{DBPath: filepath.Join(t.TempDir(), "sync.db")})
		require.Error(t, err, "no token and no tenants must be refused")
	})

	t.Run("both is refused", func(t *testing.T) {
		_, err := New(Options{
			DBPath:  filepath.Join(t.TempDir(), "sync.db"),
			Token:   "tok",
			Tenants: map[string]string{"alice": "alice-tok"},
		})
		require.Error(t, err, "a token plus named tenants must be refused")
	})

	t.Run("named tenants create no db at DBPath", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "sync.db")
		s, err := New(Options{DBPath: dbPath, Tenants: map[string]string{"alice": "alice-tok"}})
		require.NoError(t, err, "New with named tenants only")
		t.Cleanup(func() { _ = s.Close() })

		_, err = os.Stat(dbPath)
		require.Truef(t, os.IsNotExist(err), "nothing may be created at DBPath: %v", err)
		_, err = os.Stat(filepath.Join(filepath.Dir(dbPath), "tenants", "alice.db"))
		require.NoError(t, err, "alice's db must exist")
	})

	t.Run("single token creates the db at DBPath", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "sync.db")
		s, err := New(Options{DBPath: dbPath, Token: "tok"})
		require.NoError(t, err, "New with a single token")
		t.Cleanup(func() { _ = s.Close() })

		_, err = os.Stat(dbPath)
		require.NoError(t, err, "the db must exist at DBPath")
		_, err = os.Stat(filepath.Join(filepath.Dir(dbPath), "tenants"))
		require.Truef(t, os.IsNotExist(err), "no tenants dir for a single-token server: %v", err)
	})
}

// TestTenantIsolation is the security boundary: one tenant's data must never be
// visible to another tenant's device, and each lives in its own file on disk.
func TestTenantIsolation(t *testing.T) {
	alice, bob, _, dbPath := multiTenant(t)

	// alice pushes records.
	status, body := alice.do("POST", "/v1/records", wire.PushReq{HostID: "hostA", Records: mkRecords(1, 3)})
	require.Equalf(t, http.StatusOK, status, "alice push: body %s", body)

	// bob sees only his OWN device, and NO hosts, NO records: a separate db.
	status, body = bob.do("GET", "/v1/devices", nil)
	require.Equalf(t, http.StatusOK, status, "bob list devices: body %s", body)
	for _, d := range mustJSON[[]wire.Device](t, body) {
		require.NotEqual(t, alice.devID, d.ID, "bob must not see alice's device")
	}

	status, body = bob.do("GET", "/v1/hosts", nil)
	require.Equalf(t, http.StatusOK, status, "bob hosts: body %s", body)
	require.Empty(t, mustJSON[wire.HostsResp](t, body).Hosts, "bob must not see alice's hosts")

	status, body = bob.do("GET", "/v1/records?host_id=hostA", nil)
	require.Equalf(t, http.StatusOK, status, "bob pull: body %s", body)
	require.Empty(t, mustJSON[wire.PullResp](t, body).Records, "bob must not see alice's records")

	// alice still sees her own device and records.
	status, body = alice.do("GET", "/v1/devices", nil)
	require.Equalf(t, http.StatusOK, status, "alice list devices: body %s", body)
	require.Len(t, mustJSON[[]wire.Device](t, body), 1, "alice sees only her own device")

	status, body = alice.do("GET", "/v1/records?host_id=hostA", nil)
	require.Equalf(t, http.StatusOK, status, "alice pull: body %s", body)
	require.Len(t, mustJSON[wire.PullResp](t, body).Records, 3, "alice sees her records")

	// Each tenant bootstrapped its own device with no collision in the other's id
	// space, since they are different files.
	_, body = bob.do("GET", "/v1/devices", nil)
	require.Len(t, mustJSON[[]wire.Device](t, body), 1, "bob sees only his own device")

	// Both tenants are separate files under tenants/, and nothing was created at
	// the --db path itself.
	_, err := os.Stat(dbPath)
	require.Truef(t, os.IsNotExist(err), "no db at DBPath in multi-tenant mode: %v", err)
	aliceDB := filepath.Join(filepath.Dir(dbPath), "tenants", "alice.db")
	bobDB := filepath.Join(filepath.Dir(dbPath), "tenants", "bob.db")
	_, err = os.Stat(aliceDB)
	require.NoErrorf(t, err, "alice db file must exist at %s", aliceDB)
	_, err = os.Stat(bobDB)
	require.NoErrorf(t, err, "bob db file must exist at %s", bobDB)
}

// TestTenantAuth checks device routing: an unknown or absent credential is
// rejected, and each tenant's device authenticates to its own tenant only.
func TestTenantAuth(t *testing.T) {
	alice, bob, _, _ := multiTenant(t)

	bad := alice.anon().withToken("garbage")
	status, _ := bad.do("GET", "/v1/devices", nil)
	require.Equal(t, http.StatusUnauthorized, status, "unknown caller => 401")

	none := &testClient{t: t, base: alice.base}
	status, _ = none.do("GET", "/v1/devices", nil)
	require.Equal(t, http.StatusUnauthorized, status, "no credential => 401")

	status, _ = alice.do("GET", "/v1/devices", nil)
	require.Equal(t, http.StatusOK, status, "alice's device authenticates")
	status, _ = bob.do("GET", "/v1/devices", nil)
	require.Equal(t, http.StatusOK, status, "bob's device authenticates")

	// Health stays open with no token even in multi-tenant mode.
	status, _ = none.do("GET", "/v1/health", nil)
	require.Equal(t, http.StatusOK, status, "health open")
}

// TestNewBadTenantName rejects tenant names that are unsafe as filenames or
// collide with the reserved single-tenant name.
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
				Tenants: tc.tenants,
			})
			require.Error(t, err, "expected error for tenant name")
		})
	}
}

// TestNewTenantTokenErrors rejects an empty or duplicate tenant token.
func TestNewTenantTokenErrors(t *testing.T) {
	dir := t.TempDir()
	_, err := New(Options{DBPath: filepath.Join(dir, "a.db"), Tenants: map[string]string{"alice": ""}})
	require.Error(t, err, "empty tenant token must be rejected")

	_, err = New(Options{
		DBPath:  filepath.Join(dir, "b.db"),
		Tenants: map[string]string{"alice": "dup", "bob": "dup"},
	})
	require.Error(t, err, "two tenants sharing a token must be rejected")
}

// TestMustDBMissingContext proves mustDB fails closed (500) when no tenant db is
// bound: it must never fall back to a shared db.
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
	alice, bob, s, dbPath := multiTenant(t)

	alice.do("POST", "/v1/records", wire.PushReq{HostID: "ha", Records: mkRecords(1, 2)})
	bob.do("POST", "/v1/records", wire.PushReq{HostID: "hb", Records: mkRecords(1, 4)})

	s.backupAll()

	base := filepath.Dir(dbPath)
	cases := []struct {
		tenant    string
		ownHost   string
		ownN      int
		otherHost string
	}{
		{"alice", "records:ha", 2, "records:hb"},
		{"bob", "records:hb", 4, "records:ha"},
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

// TestBackupSingleTenant covers the single-token server's backup path: its one
// tenant snapshots under backups/default/.
func TestBackupSingleTenant(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sync.db")
	s, err := New(Options{DBPath: dbPath, Token: "tok"})
	require.NoError(t, err, "New single-token")
	t.Cleanup(func() { _ = s.Close() })

	s.backupAll()

	entries, err := os.ReadDir(filepath.Join(filepath.Dir(dbPath), "backups", soleTenant))
	require.NoError(t, err, "read backups/default")
	found := false
	for _, e := range entries {
		if _, ok := backupTimestamp(e.Name()); ok {
			found = true
		}
	}
	require.True(t, found, "expected a snapshot under backups/default")
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

	aliceDir := filepath.Join(filepath.Dir(dbPath), "backups", "alice")
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(aliceDir)
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
