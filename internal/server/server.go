// Package server is yore's sync server. It stores ONLY ciphertext (sealed
// record blobs, HK wraps, DEK wraps) and device public keys; it can never
// decrypt anything. Storage is a single bbolt file owned solely by this
// process. The HTTP API is defined by package internal/wire.
package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
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

// Options configures a Server.
type Options struct {
	DBPath string
	Token  string // bearer token; empty = refuse to start
}

// Server is an open sync server backed by a bbolt database.
type Server struct {
	db     *bbolt.DB
	token  string
	nonces *nonceCache
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
	log.Printf("signature rejected: %s %s device=%q: %v", r.Method, r.URL.Path, reqsign.Device(r.Header), cause)
	writeErr(w, http.StatusUnauthorized, "unauthorized")
}

// requireSignature verifies a mutating request's Ed25519 signature and records
// its nonce, on top of the bearer token already checked by the auth middleware.
// The verification key is selfKey when non-nil — a self-signed registration, or
// the first device forming the group (see bootstrapSelfKey); otherwise the
// signer named in the headers is looked up in the devices bucket and must be
// active. On any failure it writes a 401 and returns false. `target` is always
// r.URL.RequestURI(), matching the client and reqsign's canonical string.
func (s *Server) requireSignature(w http.ResponseWriter, r *http.Request, body, selfKey []byte) bool {
	deviceID := reqsign.Device(r.Header)
	if deviceID == "" {
		s.denySig(w, r, errNoSigner)
		return false
	}
	pub := selfKey
	if pub == nil {
		var dev wire.Device
		found := false
		if err := s.db.View(func(tx *bbolt.Tx) error {
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

// New opens (creating if needed) the database and ensures the fixed buckets.
// An empty token is refused so the server is never accidentally open.
func New(opts Options) (*Server, error) {
	if opts.Token == "" {
		return nil, errors.New("server: empty token; refusing to start")
	}
	db, err := bbolt.Open(opts.DBPath, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, err
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketDevices, bucketHKWraps, bucketDEKWraps, bucketMeta} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Server{db: db, token: opts.Token, nonces: newNonceCache()}, nil
}

// Close releases the database and its lock.
func (s *Server) Close() error { return s.db.Close() }

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
	defer s.Close()

	srv := &http.Server{Addr: listen, Handler: s.Handler()}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

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
		return err
	case <-ctx.Done():
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

func (s *Server) auth(next http.Handler) http.Handler {
	want := []byte("Bearer " + s.token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			next.ServeHTTP(w, r)
			return
		}
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
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
