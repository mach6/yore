// Package server is yore's sync server. It stores ONLY ciphertext (sealed
// record blobs, HK wraps, DEK wraps) and device public keys; it can never
// decrypt anything. It is multi-tenant: the bearer token selects a tenant, and
// each tenant is an isolated bbolt file owned solely by this process (the
// default tenant uses Options.DBPath; named tenants shard beside it), so tenants
// never see each other's data. The HTTP API is defined by package internal/wire.
package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"syscall"
	"time"

	"go.etcd.io/bbolt"

	"yore/internal/reqsign"
	"yore/internal/wire"
)

// Bucket names.
var (
	bucketDevices  = []byte("devices")   // deviceID -> wire.Device JSON
	bucketHKWraps  = []byte("hk_wraps")  // deviceID -> wire.HKWrap JSON
	bucketDEKWraps = []byte("dek_wraps") // keyID -> wire.DEKWrap JSON
	bucketMeta     = []byte("meta")      // k/v (hk_version)
)

// recordsPrefix names one append-only host stream bucket: recordsPrefix+hostID.
const recordsPrefix = "records:"

// metaHKVersion is the meta key holding the current HK version (decimal string).
const metaHKVersion = "hk_version"

// maxBody caps every request body.
const maxBody = 10 << 20 // 10 MiB

// defaultTenant is the reserved name of the tenant backed by Options.DBPath. It
// is also the subdirectory its rolling backups land in.
const defaultTenant = "default"

// defaultBackupKeep is used when backups are enabled but BackupKeep is unset.
const defaultBackupKeep = 3

// tenantNameRE constrains named tenants: they become filenames, so no path
// separators, dots, or other surprises are allowed.
var tenantNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Options configures a Server.
type Options struct {
	DBPath  string            // the default tenant's bbolt file
	Token   string            // the default tenant's bearer token; empty = refuse to start
	Tenants map[string]string // named tenant -> bearer token (sharded under dir(DBPath)/tenants/)

	BackupInterval time.Duration // rolling per-tenant backups every interval; 0 disables
	BackupKeep     int           // backups retained per tenant (default 3 when enabled)
}

// tenant is one isolated group's storage: its own bbolt file holding that
// group's devices, HK wraps, DEK wraps, and record streams. Tenants never share
// a file, so they can never see each other's data.
type tenant struct {
	name string
	db   *bbolt.DB
}

// tokenTenant binds a full "Bearer <token>" header value to the tenant it
// selects. Tokens are matched with a constant-time compare in the auth
// middleware, one entry per tenant.
type tokenTenant struct {
	header []byte
	tenant *tenant
}

// Server is an open sync server. Each tenant has its own bbolt file; the bearer
// token on a request selects which one every handler operates on.
type Server struct {
	tenants        []*tenant     // all open tenants, default first
	byToken        []tokenTenant // token header -> tenant (constant-time matched)
	dbPath         string        // Options.DBPath; roots the backups/ and tenants/ dirs
	backupInterval time.Duration
	backupKeep     int
	nonces         *nonceCache // shared: device IDs are globally-unique ULIDs
}

// ctxKey is the private type for request-context values so no other package can
// collide with (or read) the per-request tenant binding.
type ctxKey int

const (
	ctxKeyDB     ctxKey = iota // the tenant's *bbolt.DB
	ctxKeyTenant               // the tenant's name (string)
)

// dbFromContext returns the tenant db the auth middleware bound to r, or nil if
// none (which must be treated as an error, never a fallback — see mustDB).
func dbFromContext(r *http.Request) *bbolt.DB {
	db, _ := r.Context().Value(ctxKeyDB).(*bbolt.DB)
	return db
}

// tenantFromContext returns the tenant name bound to r (empty if none).
func tenantFromContext(r *http.Request) string {
	name, _ := r.Context().Value(ctxKeyTenant).(string)
	return name
}

// mustDB returns the tenant db bound to r by the auth middleware. Past auth it
// is always present; a nil db would be a bug, so it fails the request 500 rather
// than silently reaching for another tenant's storage — a cross-tenant leak
// would be far worse than an error.
func mustDB(w http.ResponseWriter, r *http.Request) (*bbolt.DB, bool) {
	db := dbFromContext(r)
	if db == nil {
		writeErr(w, http.StatusInternalServerError, "internal error")
		return nil, false
	}
	return db, true
}

// storedRecord is the on-disk value of a record in a host stream bucket. The
// seq is the bucket key (8-byte big-endian), not stored in the value.
type storedRecord struct {
	ID        string `json:"id"`
	KeyID     string `json:"key_id"`
	Blob      []byte `json:"blob"`
	CreatedMs int64  `json:"created_ms"`
}

// apiError carries an HTTP status and message out of a bbolt transaction.
type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func fail(status int, msg string) *apiError { return &apiError{status: status, msg: msg} }

// Signature-check failure causes. These are logged server-side only; the client
// always receives a generic 401 so it never learns which check failed.
var (
	errNoSigner       = errors.New("missing device header")
	errInactiveSigner = errors.New("signer is not an active device")
	errReplay         = errors.New("replayed request")
	errDeviceMismatch = errors.New("signer device id != registration id")
)

// ---- signature verification ----

// nonceCache rejects a repeated (device, nonce) pair within reqsign.Skew. A
// TLS-inspecting proxy that captures a fully valid signed request can otherwise
// replay it verbatim; the signature stays valid, so replay defence lives here.
// It is keyed by device+"\x00"+nonce and, because a nonce is only meaningful for
// reqsign.Skew (after which reqsign.Verify rejects the stale timestamp anyway),
// stays bounded by the request rate over that 5-minute window — tiny for a
// single-user fleet. Expired entries are swept lazily on each insert.
type nonceCache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

func newNonceCache() *nonceCache { return &nonceCache{seen: make(map[string]time.Time)} }

// checkAndRecord records (device, nonce) and returns true when it is fresh; it
// returns false without recording anything when the pair was already seen within
// reqsign.Skew.
func (n *nonceCache) checkAndRecord(device, nonce string, now time.Time) bool {
	key := device + "\x00" + nonce
	n.mu.Lock()
	defer n.mu.Unlock()
	for k, ts := range n.seen {
		if now.Sub(ts) > reqsign.Skew {
			delete(n.seen, k)
		}
	}
	if _, ok := n.seen[key]; ok {
		return false
	}
	n.seen[key] = now
	return true
}

// readBody reads the (already size-capped) request body once so it can be both
// signature-verified and JSON-decoded, then restores r.Body so the handler's
// decodeJSON still works. The 10 MiB cap is enforced upstream by limitBody's
// MaxBytesReader.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "unable to read body")
		return nil, false
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, true
}

// denySig logs the specific signature failure for operators and returns a
// generic 401, leaking nothing about which check failed.
func (s *Server) denySig(w http.ResponseWriter, r *http.Request, cause error) {
	log.Printf("signature rejected: tenant=%q %s %s device=%q: %v",
		tenantFromContext(r), r.Method, r.URL.Path, reqsign.Device(r.Header), cause)
	writeErr(w, http.StatusUnauthorized, "unauthorized")
}

// requireSignature verifies a mutating request's Ed25519 signature and records
// its nonce, on top of the bearer token already checked by the auth middleware.
// The verification key is selfKey when non-nil — a self-signed registration, or
// the first device forming the group (see bootstrapSelfKey); otherwise the
// signer named in the headers is looked up in the devices bucket and must be
// active. On any failure it writes a 401 and returns false. `target` is always
// r.URL.RequestURI(), matching the client and reqsign's canonical string.
func (s *Server) requireSignature(w http.ResponseWriter, r *http.Request, db *bbolt.DB, body, selfKey []byte) bool {
	deviceID := reqsign.Device(r.Header)
	if deviceID == "" {
		s.denySig(w, r, errNoSigner)
		return false
	}
	pub := selfKey
	if pub == nil {
		var dev wire.Device
		found := false
		if err := db.View(func(tx *bbolt.Tx) error {
			raw := tx.Bucket(bucketDevices).Get([]byte(deviceID))
			if raw == nil {
				return nil
			}
			found = true
			return json.Unmarshal(raw, &dev)
		}); err != nil {
			writeErr(w, http.StatusInternalServerError, "internal error")
			return false
		}
		if !found || dev.Status != wire.DeviceActive {
			s.denySig(w, r, errInactiveSigner)
			return false
		}
		pub = dev.SignKey
	}
	if err := reqsign.Verify(r.Header, r.Method, r.URL.RequestURI(), body, pub, time.Now()); err != nil {
		s.denySig(w, r, err)
		return false
	}
	if !s.nonces.checkAndRecord(deviceID, reqsign.Nonce(r.Header), time.Now()) {
		s.denySig(w, r, errReplay)
		return false
	}
	return true
}

// validateTenantName rejects anything that would be unsafe as a filename or
// collide with the reserved default tenant. Named tenants become
// dir(DBPath)/tenants/<name>.db, so only [A-Za-z0-9_-]+ is allowed.
func validateTenantName(name string) error {
	if name == "" {
		return errors.New("server: empty tenant name")
	}
	if name == defaultTenant {
		return fmt.Errorf("server: tenant name %q is reserved", name)
	}
	if !tenantNameRE.MatchString(name) {
		return fmt.Errorf("server: invalid tenant name %q (allowed: A-Za-z0-9_-)", name)
	}
	return nil
}

// openTenantDB opens (creating if needed) one tenant's bbolt file and ensures
// the fixed buckets.
func openTenantDB(path string) (*bbolt.DB, error) {
	db, err := bbolt.Open(path, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketDevices, bucketHKWraps, bucketDEKWraps, bucketMeta} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// New opens every tenant's database (creating and initialising each), and maps
// each bearer token to its tenant. The default tenant is Options.DBPath keyed by
// Options.Token; every entry of Options.Tenants is a named tenant sharded at
// dir(DBPath)/tenants/<name>.db. An empty default token is refused so the server
// is never accidentally open. Tenant dbs are opened eagerly (the count is small)
// so the backup loop can cover every one and no request pays an open cost.
func New(opts Options) (*Server, error) {
	if opts.Token == "" {
		return nil, errors.New("server: empty token; refusing to start")
	}

	// Build the tenant table up front, validating names and rejecting duplicate
	// tokens (two tenants sharing a token would be indistinguishable — a leak).
	type spec struct{ name, path, token string }
	specs := []spec{{name: defaultTenant, path: opts.DBPath, token: opts.Token}}
	seenTokens := map[string]bool{opts.Token: true}
	baseDir := filepath.Dir(opts.DBPath)
	for name, token := range opts.Tenants {
		if err := validateTenantName(name); err != nil {
			return nil, err
		}
		if token == "" {
			return nil, fmt.Errorf("server: tenant %q has an empty token", name)
		}
		if seenTokens[token] {
			return nil, fmt.Errorf("server: tenant %q reuses another tenant's token", name)
		}
		seenTokens[token] = true
		specs = append(specs, spec{name: name, path: filepath.Join(baseDir, "tenants", name+".db"), token: token})
	}
	if len(opts.Tenants) > 0 {
		if err := os.MkdirAll(filepath.Join(baseDir, "tenants"), 0o700); err != nil {
			return nil, err
		}
	}

	s := &Server{
		dbPath:         opts.DBPath,
		backupInterval: opts.BackupInterval,
		backupKeep:     opts.BackupKeep,
		nonces:         newNonceCache(),
	}
	for _, sp := range specs {
		db, err := openTenantDB(sp.path)
		if err != nil {
			_ = s.Close() // close any tenants already opened
			return nil, err
		}
		t := &tenant{name: sp.name, db: db}
		s.tenants = append(s.tenants, t)
		s.byToken = append(s.byToken, tokenTenant{header: []byte("Bearer " + sp.token), tenant: t})
	}
	return s, nil
}

// Close releases every tenant database and its lock, returning the first error.
func (s *Server) Close() error {
	var firstErr error
	for _, t := range s.tenants {
		if err := t.db.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Handler returns the full mux with logging + auth + body-limit middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/hosts", s.handleHosts)
	mux.HandleFunc("POST /v1/records", s.handlePush)
	mux.HandleFunc("GET /v1/records", s.handlePull)
	mux.HandleFunc("POST /v1/devices", s.handleRegister)
	mux.HandleFunc("GET /v1/devices", s.handleListDevices)
	mux.HandleFunc("POST /v1/devices/{id}/activate", s.handleActivate)
	mux.HandleFunc("POST /v1/devices/{id}/revoke", s.handleRevoke)
	mux.HandleFunc("GET /v1/keys/hk", s.handleGetHK)
	mux.HandleFunc("GET /v1/keys/dek", s.handleListDEK)
	mux.HandleFunc("POST /v1/keys/dek", s.handlePostDEK)
	mux.HandleFunc("POST /v1/keys/rotate", s.handleRotate)
	return s.logging(s.limitBody(s.auth(mux)))
}

// Run opens a Server and serves it, shutting down gracefully on SIGINT/SIGTERM.
func Run(opts Options, listen string) error {
	s, err := New(opts)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	srv := &http.Server{Addr: listen, Handler: s.Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Optional rolling per-tenant backups. A single goroutine, stopped before the
	// dbs are closed so a snapshot never races Close.
	var wg sync.WaitGroup
	backupDone := make(chan struct{})
	if s.backupInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.backupLoop(backupDone)
		}()
	}
	stopBackups := func() {
		close(backupDone)
		wg.Wait()
	}

	errCh := make(chan error, 1)
	go func() {
		err := srv.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case err := <-errCh:
		stopBackups()
		return err
	case <-ctx.Done():
		stopBackups()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// ---- middleware ----

// statusRecorder captures the response status for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("%s %s %d %dms", r.Method, r.URL.Path, rec.status, time.Since(start).Milliseconds())
	})
}

func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		}
		next.ServeHTTP(w, r)
	})
}

// auth resolves the bearer token to a tenant and binds that tenant's db (and
// name) into the request context for the handlers. /v1/health stays open. The
// token is compared constant-time against every tenant's token without an early
// break, so a match leaks nothing about which tenant (or how many) exist.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			next.ServeHTTP(w, r)
			return
		}
		got := []byte(r.Header.Get("Authorization"))
		var matched *tenant
		for i := range s.byToken {
			if subtle.ConstantTimeCompare(got, s.byToken[i].header) == 1 {
				matched = s.byToken[i].tenant
			}
		}
		if matched == nil {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyDB, matched.db)
		ctx = context.WithValue(ctx, ctxKeyTenant, matched.name)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, wire.ErrorResp{Error: msg})
}

// writeAPIErr maps an error out of a transaction to its status, or 500.
func writeAPIErr(w http.ResponseWriter, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeErr(w, ae.status, ae.msg)
		return
	}
	writeErr(w, http.StatusInternalServerError, "internal error")
}

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

// ---- storage helpers ----

func seqKey(seq uint64) []byte {
	var k [8]byte
	binary.BigEndian.PutUint64(k[:], seq)
	return k[:]
}

func beUint64(b []byte) uint64 {
	if len(b) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(b)
}

func getHKVersion(tx *bbolt.Tx) int {
	v := tx.Bucket(bucketMeta).Get([]byte(metaHKVersion))
	if v == nil {
		return 0
	}
	n, _ := strconv.Atoi(string(v))
	return n
}

func setHKVersion(tx *bbolt.Tx, version int) error {
	return tx.Bucket(bucketMeta).Put([]byte(metaHKVersion), []byte(strconv.Itoa(version)))
}
