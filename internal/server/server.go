// Package server is yore's sync server. It stores ONLY ciphertext (sealed record
// blobs, HK wraps, DEK wraps) and device public keys; it can never decrypt
// anything. A server hosts either exactly one tenant (Options.Token, its db at
// Options.DBPath) or a set of named tenants (Options.Tenants, sharded beside
// it); never both. Each tenant is an isolated bbolt file owned solely by this
// process, so tenants never see each other's data. The HTTP API is defined by
// package internal/wire.
package server

import (
	"bytes"
	"context"
	"crypto/sha256"
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
	"sort"
	"strconv"
	"strings"
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
	bucketTokens   = []byte("tokens")    // sha256(token) -> token JSON
	bucketRecovery = []byte("recovery")  // fixed key -> wire.RecoveryInit JSON
)

// recoveryKey is the single key under which bucketRecovery stores the group's
// recovery material.
var recoveryKey = []byte("recovery")

// recordsPrefix names one append-only host stream bucket: recordsPrefix+hostID.
const recordsPrefix = "records:"

// metaHKVersion is the meta key holding the current HK version (decimal string).
const metaHKVersion = "hk_version"

// metaReadyProbe is the meta key the readiness probe writes. Its value (a
// unix-millis stamp) is never read back: what is being tested is the commit,
// not the contents.
const metaReadyProbe = "ready_probe"

// readyProbeTTL bounds how often /v1/ready actually touches storage. The
// endpoint is open, so without a cached verdict anyone who can reach the server
// could turn it into a write amplifier.
const readyProbeTTL = 10 * time.Second

// maxBody caps every request body.
const maxBody = 10 << 20 // 10 MiB

// soleTenant is the name of the one tenant a single-token server hosts, backed
// by Options.DBPath. It is also the subdirectory its rolling backups land in,
// which is why it stays reserved for named tenants too: a tenant called
// "default" would drop its snapshots into backups/default/ beside those of a
// different db, and a restore could then pick the wrong file.
const soleTenant = "default"

// defaultBackupKeep is used when backups are enabled but BackupKeep is unset.
const defaultBackupKeep = 3

// tenantNameRE constrains named tenants: they become filenames, so no path
// separators, dots, or other surprises are allowed.
var tenantNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// Options configures a Server. Exactly one of Token (one tenant, its db at
// DBPath) or Tenants (named tenants, sharded beside it) must be set: neither is
// a server with no way in, both is two answers to "where does this token's data
// live", so New refuses either way.
type Options struct {
	DBPath  string            // one tenant: its bbolt file. Named tenants: only roots tenants/ and backups/
	Token   string            // the single tenant's bearer token; mutually exclusive with Tenants
	Tenants map[string]string // named tenant -> bearer token, at dir(DBPath)/tenants/<name>.db

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

// tokenTenant binds a tenant's configured bootstrap token to that tenant. The
// token is no longer an API credential: it only authorizes forming a group that
// has no active device yet (see tenantForToken).
type tokenTenant struct {
	token  []byte
	tenant *tenant
}

// hdrToken carries a single-use enrollment token on a registration request.
const hdrToken = "X-Yore-Token"

// tokenTTL bounds how long a minted enrollment token stays usable.
const tokenTTL = 30 * time.Minute

// tokenKeep is how long an unclaimed token is kept past its expiry so it can
// still be listed. A claimed one is never pruned: it is the record of an
// enrollment, and "which token admitted this machine" stops being answerable
// the moment it is thrown away.
const tokenKeep = 7 * 24 * time.Hour

// storedToken is one minted enrollment token. Only the hash of the token is a
// key in the bucket, so the server never holds a usable token at rest: which
// is also why a token can never be shown again after it is minted.
type storedToken struct {
	CreatedMs int64  `json:"created_ms"`
	ExpiresMs int64  `json:"expires_ms"`
	ClaimedMs int64  `json:"claimed_ms,omitempty"` // 0 = never claimed
	ClaimedBy string `json:"claimed_by,omitempty"` // device id that enrolled on it
	RevokedMs int64  `json:"revoked_ms,omitempty"` // 0 = not revoked
}

// state classifies a token for display. Claimed and revoked are terminal and
// outrank expiry: a token that was used at minute 2 reads "claimed" forever,
// not "expired" from minute 30.
func (st storedToken) state(now time.Time) string {
	switch {
	case st.ClaimedMs != 0:
		return wire.TokenClaimed
	case st.RevokedMs != 0:
		return wire.TokenRevoked
	case now.UnixMilli() >= st.ExpiresMs:
		return wire.TokenExpired
	default:
		return wire.TokenOpen
	}
}

// usable reports whether this token may still admit a machine.
func (st storedToken) usable(now time.Time) bool { return st.state(now) == wire.TokenOpen }

// hashToken maps a token to its storage key.
func hashToken(token string) []byte {
	sum := sha256.Sum256([]byte("yore/token/v1|" + token))
	return sum[:]
}

// loadToken reads one stored token by its bucket key.
func loadToken(tx *bbolt.Tx, sum []byte) (storedToken, bool) {
	raw := tx.Bucket(bucketTokens).Get(sum)
	if raw == nil {
		return storedToken{}, false
	}
	var st storedToken
	if json.Unmarshal(raw, &st) != nil {
		return storedToken{}, false
	}
	return st, true
}

// putToken writes one stored token back under its bucket key.
func putToken(tx *bbolt.Tx, sum []byte, st storedToken) error {
	val, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return tx.Bucket(bucketTokens).Put(sum, val)
}

// tokenValid reports whether sum names a token that may still be used.
func tokenValid(tx *bbolt.Tx, sum []byte, now time.Time) bool {
	st, ok := loadToken(tx, sum)
	return ok && st.usable(now)
}

// redeemToken marks a token claimed by the device enrolling on it. Redemption
// is single-use and happens in the same transaction as the device registration
// it authorizes, so two devices can never enroll on one token.
func redeemToken(tx *bbolt.Tx, sum []byte, now time.Time, deviceID string) error {
	st, ok := loadToken(tx, sum)
	if !ok || !st.usable(now) {
		return fail(http.StatusUnauthorized, "unauthorized")
	}
	st.ClaimedMs, st.ClaimedBy = now.UnixMilli(), deviceID
	return putToken(tx, sum, st)
}

// Server is an open sync server. Each tenant has its own bbolt file; the bearer
// token on a request selects which one every handler operates on.
type Server struct {
	tenants        []*tenant     // all open tenants (one, or every named tenant in name order)
	byToken        []tokenTenant // bootstrap token -> tenant (constant-time matched)
	dbPath         string        // Options.DBPath; roots the backups/ and tenants/ dirs
	backupInterval time.Duration
	backupKeep     int
	nonces         *nonceCache // shared: device IDs are globally-unique ULIDs

	// route caches device id -> tenant so routing costs one map read rather than
	// a scan of every tenant's device bucket per request.
	routeMu sync.RWMutex
	route   map[string]*tenant

	// ready caches the last storage probe (see probeStorage).
	readyMu  sync.Mutex
	readyAt  time.Time
	readyErr error
}

// probeStorage reports whether every tenant db can still commit a write,
// caching the verdict for readyProbeTTL. A bbolt read is served from the
// existing mmap and needs no write at all, so a server whose volume has filled
// (or gone read-only, or started failing I/O) keeps answering every GET
// perfectly while every mutation fails: the archive looks alive and silently
// stops accepting history. Nothing short of an actual commit detects that,
// which is why this writes.
func (s *Server) probeStorage(now time.Time) error {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if !s.readyAt.IsZero() && now.Sub(s.readyAt) < readyProbeTTL {
		return s.readyErr
	}
	var probeErr error
	for _, t := range s.tenants {
		err := t.db.Update(func(tx *bbolt.Tx) error {
			return tx.Bucket(bucketMeta).Put([]byte(metaReadyProbe),
				[]byte(strconv.FormatInt(now.UnixMilli(), 10)))
		})
		if err != nil {
			probeErr = fmt.Errorf("tenant %q: %w", t.name, err)
			break
		}
	}
	// Log only the transitions: a probe that keeps failing is one incident, and
	// a recovery is the line an operator looks for after fixing the disk.
	switch {
	case probeErr != nil && s.readyErr == nil:
		log.Printf("storage unwritable: %v", probeErr)
	case probeErr == nil && s.readyErr != nil:
		log.Printf("storage writable again")
	}
	s.readyAt, s.readyErr = now, probeErr
	return probeErr
}

// ctxKey is the private type for request-context values so no other package can
// collide with (or read) the per-request tenant binding.
type ctxKey int

const (
	ctxKeyDB     ctxKey = iota // the tenant's *bbolt.DB
	ctxKeyTenant               // the tenant's name (string)
)

// dbFromContext returns the tenant db the auth middleware bound to r, or nil if
// none (which must be treated as an error, never a fallback; see mustDB).
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
// than silently reaching for another tenant's storage: a cross-tenant leak would
// be far worse than an error.
func mustDB(w http.ResponseWriter, r *http.Request) (*bbolt.DB, bool) {
	db := dbFromContext(r)
	if db == nil {
		logInternal(w, r, errors.New("no tenant bound to the request"))
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
// stays bounded by the request rate over that 5-minute window; tiny for a
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
		// A body over the limit reads as a plain error, which would report as a
		// malformed request and tell the client nothing about how to recover.
		// Name it: 413 is what makes a client's batch-splitting retry correct
		// rather than a guess.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("request body exceeds %d bytes", tooLarge.Limit))
			return nil, false
		}
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
// The verification key is selfKey when non-nil: a self-signed registration, or
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
	var dev wire.Device
	known := false
	if pub == nil {
		if err := db.View(func(tx *bbolt.Tx) error {
			raw := tx.Bucket(bucketDevices).Get([]byte(deviceID))
			if raw == nil {
				return nil
			}
			known = true
			return json.Unmarshal(raw, &dev)
		}); err != nil {
			logInternal(w, r, fmt.Errorf("read device %q: %w", deviceID, err))
			return false
		}
		// An unknown device is refused without a hint: there is no key to check a
		// signature against, and saying which ids exist would enumerate the group.
		if !known {
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
	// Status is checked AFTER the signature, so only the holder of this device's
	// private key learns its standing, and a revoked one is told exactly that,
	// which is the only way it can find out it should stop syncing and drop the
	// group's ciphertext. A device awaiting approval is refused without the
	// distinction: "pending" is a state it already knows it is in.
	if known && dev.Status != wire.DeviceActive {
		if dev.Status == wire.DeviceRevoked {
			log.Printf("revoked device refused: tenant=%q %s %s device=%q",
				tenantFromContext(r), r.Method, r.URL.Path, deviceID)
			writeCodedErr(w, http.StatusForbidden, wire.CodeDeviceRevoked, "device revoked")
			return false
		}
		s.denySig(w, r, errInactiveSigner)
		return false
	}
	return true
}

// validateTenantName rejects anything that would be unsafe as a filename or
// collide with the reserved single-tenant name. Named tenants become
// dir(DBPath)/tenants/<name>.db, so only [A-Za-z0-9_-]+ is allowed.
func validateTenantName(name string) error {
	if name == "" {
		return errors.New("server: empty tenant name")
	}
	if name == soleTenant {
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
		for _, name := range [][]byte{bucketDevices, bucketHKWraps, bucketDEKWraps, bucketMeta, bucketTokens, bucketRecovery} {
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
// each bearer token to its tenant. With Options.Token set the server hosts one
// tenant, its db at Options.DBPath. With Options.Tenants set each entry is a
// named tenant sharded at dir(DBPath)/tenants/<name>.db and nothing is created at
// DBPath itself. Setting both, or neither, is refused. Tenant dbs are opened
// eagerly (the count is small) so the backup loop can cover every one and no
// request pays an open cost.
func New(opts Options) (*Server, error) {
	switch {
	case opts.Token == "" && len(opts.Tenants) == 0:
		return nil, errors.New("server: no token configured; refusing to start")
	case opts.Token != "" && len(opts.Tenants) != 0:
		return nil, errors.New("server: Token and Tenants are mutually exclusive; configure one token or a set of named tenants")
	}

	// Build the tenant table up front, validating names and rejecting duplicate
	// tokens (two tenants sharing a token would be indistinguishable: a leak).
	// Named tenants are ordered so startup, and any error it reports, does not
	// depend on map iteration order.
	type spec struct{ name, path, token string }
	var specs []spec
	baseDir := filepath.Dir(opts.DBPath)
	if opts.Token != "" {
		specs = append(specs, spec{name: soleTenant, path: opts.DBPath, token: opts.Token})
	} else {
		names := make([]string, 0, len(opts.Tenants))
		for name := range opts.Tenants {
			names = append(names, name)
		}
		sort.Strings(names)
		tokenOwner := make(map[string]string, len(names))
		for _, name := range names {
			token := opts.Tenants[name]
			if err := validateTenantName(name); err != nil {
				return nil, err
			}
			if token == "" {
				return nil, fmt.Errorf("server: tenant %q has an empty token", name)
			}
			if owner, dup := tokenOwner[token]; dup {
				return nil, fmt.Errorf("server: tenants %q and %q share a token", owner, name)
			}
			tokenOwner[token] = name
			specs = append(specs, spec{name: name, path: filepath.Join(baseDir, "tenants", name+".db"), token: token})
		}
		if err := os.MkdirAll(filepath.Join(baseDir, "tenants"), 0o700); err != nil {
			return nil, err
		}
	}

	s := &Server{
		dbPath:         opts.DBPath,
		backupInterval: opts.BackupInterval,
		backupKeep:     opts.BackupKeep,
		nonces:         newNonceCache(),
		route:          make(map[string]*tenant),
	}
	for _, sp := range specs {
		db, err := openTenantDB(sp.path)
		if err != nil {
			_ = s.Close() // close any tenants already opened
			return nil, err
		}
		t := &tenant{name: sp.name, db: db}
		s.tenants = append(s.tenants, t)
		s.byToken = append(s.byToken, tokenTenant{token: []byte(sp.token), tenant: t})
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
	mux.HandleFunc("GET /v1/ready", s.handleReady)

	// Reads are signed exactly like mutations: with no bearer token, the device
	// key is the only credential there is.
	mux.HandleFunc("GET /v1/hosts", s.signed(s.handleHosts))
	mux.HandleFunc("GET /v1/records", s.signed(s.handlePull))
	mux.HandleFunc("GET /v1/devices", s.signed(s.handleListDevices))
	mux.HandleFunc("GET /v1/keys/hk", s.signed(s.handleGetHK))
	mux.HandleFunc("GET /v1/keys/dek", s.signed(s.handleListDEK))

	mux.HandleFunc("POST /v1/records", s.handlePush)
	mux.HandleFunc("POST /v1/devices", s.handleRegister)
	mux.HandleFunc("POST /v1/devices/{id}/activate", s.handleActivate)
	mux.HandleFunc("POST /v1/devices/{id}/revoke", s.handleRevoke)
	mux.HandleFunc("POST /v1/keys/dek", s.handlePostDEK)
	mux.HandleFunc("POST /v1/keys/rotate", s.handleRotate)
	mux.HandleFunc("POST /v1/tokens", s.handleMintToken)
	mux.HandleFunc("GET /v1/tokens", s.signed(s.handleListTokens))
	mux.HandleFunc("POST /v1/tokens/{id}/revoke", s.signed(s.handleRevokeToken))

	// Recovery runs when no device survives to authenticate. The salt is public
	// (it is an Argon2id input, not a secret) and must be readable before the
	// caller can derive the key that signs everything else.
	mux.HandleFunc("GET /v1/recovery/salt", s.handleRecoverySalt)
	mux.HandleFunc("GET /v1/recovery", s.handleGetRecovery)
	mux.HandleFunc("POST /v1/recovery", s.handleInitRecovery)
	mux.HandleFunc("POST /v1/recovery/token", s.handleRecoveryToken)
	mux.HandleFunc("POST /v1/recovery/activate/{id}", s.handleRecoveryActivate)
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

// auth binds the tenant a request belongs to into its context.
//
// There is no bearer token: an enrolled device authenticates with its Ed25519
// key alone (see requireSignature), so the tenant is resolved from the device
// named in X-Yore-Device: the device record itself is the credential, and no
// long-lived shared secret exists to leak. Requests that predate having a
// device record carry their own routing instead:
//
//   - /v1/health and /v1/ready are open.
//   - enrollment (POST /v1/devices) routes by its X-Yore-Token header.
//   - recovery routes by the recovery public key that signed it.
//
// Signature verification happens downstream, per handler; this only decides
// which tenant's database the handler operates on. A request it cannot route is
// rejected 401 with no detail about which tenants exist.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isOpenPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		var matched *tenant
		switch {
		case isEnrollPath(r):
			matched = s.tenantForToken(r.Header.Get(hdrToken))
		case isRecoveryPath(r):
			matched = s.tenantWithRecovery()
		default:
			matched = s.tenantForDevice(reqsign.Device(r.Header))
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

// isOpenPath reports whether path is one of the two endpoints that answer
// before any identity is established: liveness and readiness. Neither touches a
// tenant's history, so neither needs one.
func isOpenPath(path string) bool {
	return path == "/v1/health" || path == "/v1/ready"
}

// isEnrollPath reports whether r is a device registration, which routes by
// enrollment token rather than by an existing device record.
func isEnrollPath(r *http.Request) bool {
	return r.Method == http.MethodPost && r.URL.Path == "/v1/devices"
}

// isRecoveryPath reports whether r must route by the recovery material rather
// than by a device record: the case when no device of this group survives to
// authenticate: the recovery reads, and minting the token that lets a
// replacement machine enroll. Publishing the material (POST /v1/recovery) is
// deliberately excluded: it happens at bootstrap, when there is nothing to
// route by yet and the registering device is present to route by instead.
func isRecoveryPath(r *http.Request) bool {
	if r.Method == http.MethodPost {
		return r.URL.Path == "/v1/recovery/token" ||
			strings.HasPrefix(r.URL.Path, "/v1/recovery/activate/")
	}
	return r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/recovery")
}

// tenantForDevice finds the tenant holding a device record with this id.
// Device ids are globally-unique ULIDs, so at most one tenant can match; the
// scan is over a handful of tenants and its result is cached.
func (s *Server) tenantForDevice(deviceID string) *tenant {
	if deviceID == "" {
		return nil
	}
	s.routeMu.RLock()
	t, ok := s.route[deviceID]
	s.routeMu.RUnlock()
	if ok {
		return t
	}
	for _, cand := range s.tenants {
		found := false
		_ = cand.db.View(func(tx *bbolt.Tx) error {
			found = tx.Bucket(bucketDevices).Get([]byte(deviceID)) != nil
			return nil
		})
		if found {
			s.routeMu.Lock()
			s.route[deviceID] = cand
			s.routeMu.Unlock()
			return cand
		}
	}
	return nil
}

// tenantForToken finds the tenant that minted an unredeemed enrollment token,
// or, while a tenant has no active device at all, the one whose configured
// bootstrap token this is. The bootstrap allowance is what lets a brand-new (or
// wiped) group form; once any device is active it stops applying, so every
// later enrollment needs a token minted by an enrolled device.
func (s *Server) tenantForToken(token string) *tenant {
	if token == "" {
		return nil
	}
	sum := hashToken(token)
	for _, cand := range s.tenants {
		valid := false
		_ = cand.db.View(func(tx *bbolt.Tx) error {
			valid = tokenValid(tx, sum, time.Now())
			return nil
		})
		if valid {
			return cand
		}
	}
	// Bootstrap: the configured token enrolls only into a tenant with no active
	// device. Compared constant-time against every tenant without an early
	// break, so a match leaks nothing about which tenants exist.
	got := []byte(token)
	var matched *tenant
	for i := range s.byToken {
		if subtle.ConstantTimeCompare(got, s.byToken[i].token) == 1 {
			matched = s.byToken[i].tenant
		}
	}
	if matched != nil && !hasActiveDevice(matched.db) {
		return matched
	}
	return nil
}

// tenantWithRecovery returns the single tenant that has recovery material.
// Recovery runs when no device survives, so there is no device id to route by;
// with more than one tenant holding recovery material the request is ambiguous
// and refused rather than guessed at.
func (s *Server) tenantWithRecovery() *tenant {
	var found *tenant
	for _, cand := range s.tenants {
		has := false
		_ = cand.db.View(func(tx *bbolt.Tx) error {
			has = tx.Bucket(bucketRecovery).Get(recoveryKey) != nil
			return nil
		})
		if has {
			if found != nil {
				return nil // ambiguous
			}
			found = cand
		}
	}
	return found
}

// hasActiveDevice reports whether any device in db is active.
func hasActiveDevice(db *bbolt.DB) bool {
	active := false
	_ = db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDevices).ForEach(func(_, v []byte) error {
			var d wire.Device
			if json.Unmarshal(v, &d) == nil && d.Status == wire.DeviceActive {
				active = true
			}
			return nil
		})
	})
	return active
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

// writeCodedErr is writeErr with a machine-readable code the client acts on.
func writeCodedErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, wire.ErrorResp{Error: msg, Code: code})
}

// writeAPIErr maps an error out of a transaction to its status, or 500. An
// *apiError is a decision this package made about the request, and its message
// is the client's answer. Anything else is a storage or encoding failure the
// client can do nothing about and is deliberately told nothing about: which
// makes this the only place the cause can be recorded. The access log carries
// the status and never the reason, so without this line a failed write
// transaction (a full disk, a read-only volume, an I/O error) is invisible from
// both ends of the connection.
func writeAPIErr(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	if errors.As(err, &ae) {
		writeErr(w, ae.status, ae.msg)
		return
	}
	logInternal(w, r, err)
}

// logInternal records the cause of a 500 for operators and answers the client
// with the generic error. Every 500 in this package goes through here.
func logInternal(w http.ResponseWriter, r *http.Request, cause error) {
	log.Printf("internal error: tenant=%q %s %s: %v",
		tenantFromContext(r), r.Method, r.URL.Path, cause)
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
