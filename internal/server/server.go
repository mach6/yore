// Package server is yore's sync server. It stores ONLY ciphertext (sealed
// record blobs, HK wraps, DEK wraps) and device public keys; it can never
// decrypt anything. Storage is a single bbolt file owned solely by this
// process. The HTTP API is defined by package internal/wire.
package server

import (
	"context"
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"go.etcd.io/bbolt"

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
	db    *bbolt.DB
	token string
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
	return &Server{db: db, token: opts.Token}, nil
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
