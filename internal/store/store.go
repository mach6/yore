// Package store is yore's local bbolt database. It holds only this host's
// history stream. bbolt is single-owner, so Open takes an exclusive file
// lock; a competing opener is reported as ErrLocked rather than blocking.
package store

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"

	"yore/internal/config"
	"yore/internal/rec"
	"yore/internal/spool"
)

// ErrLocked is returned by Open when another process already holds the store.
var ErrLocked = errors.New("store: locked by another process")

// ErrSchemaNewer is returned by Open when data.db carries a schema version this
// build does not know. Refusing is the point: an older binary that guessed at a
// newer layout would misread records or write ones the newer build cannot, and
// this is the local history; the one copy of it on this machine.
var ErrSchemaNewer = errors.New("store: data.db was written by a newer version of yore")

// ErrSchemaBad is returned when the recorded schema version is not a number.
var ErrSchemaBad = errors.New("store: data.db has a malformed schema version")

var (
	bucketHistory = []byte("history") // seq (8-byte BE) -> rec.Record JSON
	bucketIDs     = []byte("ids")     // record ID -> seq (8-byte BE)
	bucketMeta    = []byte("meta")    // string key -> string value
)

const (
	metaHostID   = "host_id"
	metaHostname = "hostname"
	metaSchema   = "schema"
)

// SchemaVersion is the on-disk layout this build reads and writes. It exists to
// be READ, not yet to be migrated: there is no upgrade path, so the only thing a
// version does today is let a build refuse a database it cannot safely touch
// instead of misreading it. Stamping it now is what makes an upgrade path
// possible later: a store with no version at all leaves a future migration
// guessing from structure. A store predating this constant carries no version
// key. That is not an unknown format, it IS version 1: the layout has not
// changed, so Open stamps it rather than refusing every database that already
// exists.
const SchemaVersion = 1

// Store is an open handle to the local history database.
type Store struct {
	db       *bbolt.DB
	dir      string
	hostID   string // opaque ULID for this machine; set once, cached
	hostname string // OS hostname, refreshed each Open, cached
	schema   int    // on-disk layout version (see SchemaVersion)
}

// checkSchema reads the recorded layout version, stamping SchemaVersion when
// there is none (a new store, or one predating versioning: both are the current
// layout). A version this build does not know is refused, never guessed at.
// Caller holds a write transaction.
func checkSchema(mb *bbolt.Bucket) (int, error) {
	raw := mb.Get([]byte(metaSchema))
	if raw == nil {
		return SchemaVersion, mb.Put([]byte(metaSchema), []byte(strconv.Itoa(SchemaVersion)))
	}
	v, err := strconv.Atoi(string(raw))
	if err != nil || v < 1 {
		return 0, fmt.Errorf("%w: %q", ErrSchemaBad, raw)
	}
	if v > SchemaVersion {
		return 0, fmt.Errorf("%w (schema %d; this build understands %d). Upgrade yore",
			ErrSchemaNewer, v, SchemaVersion)
	}
	// v < SchemaVersion is where a migration would run. There is none yet, and
	// there is only one version, so this is unreachable until the layout changes.
	return v, nil
}

// Open opens (creating if needed) the store under the state dir. It creates
// the bucket layout, assigns a stable host_id on first use, and refreshes the
// cached hostname. If another process holds the lock it returns ErrLocked.
func Open(dir string) (*Store, error) {
	if err := config.EnsureDir(dir); err != nil {
		return nil, err
	}
	db, err := bbolt.Open(config.DBPath(dir), 0o600, &bbolt.Options{Timeout: 200 * time.Millisecond})
	if err != nil {
		if errors.Is(err, bolterrors.ErrTimeout) {
			return nil, ErrLocked
		}
		return nil, err
	}

	s := &Store{db: db, dir: dir}
	hostname, _ := os.Hostname()
	err = db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketHistory, bucketIDs, bucketMeta} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		mb := tx.Bucket(bucketMeta)
		schema, serr := checkSchema(mb)
		if serr != nil {
			return serr
		}
		s.schema = schema
		if mb.Get([]byte(metaHostID)) == nil {
			if err := mb.Put([]byte(metaHostID), []byte(rec.NewID())); err != nil {
				return err
			}
		}
		if hostname != "" {
			if err := mb.Put([]byte(metaHostname), []byte(hostname)); err != nil {
				return err
			}
		}
		s.hostID = string(mb.Get([]byte(metaHostID)))
		s.hostname = string(mb.Get([]byte(metaHostname)))
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the store and its lock.
func (s *Store) Close() error { return s.db.Close() }

// BackupTo writes a consistent online snapshot of the whole database to w and
// returns the number of bytes written. It runs inside a read transaction, so
// it is a hot backup: safe to call while the store is being read and written
// (bbolt's Tx.WriteTo is the hot-backup primitive). The output is a complete,
// self-contained bbolt file that Open can be pointed at directly.
func (s *Store) BackupTo(w io.Writer) (int64, error) {
	var n int64
	err := s.db.View(func(tx *bbolt.Tx) error {
		var e error
		n, e = tx.WriteTo(w)
		return e
	})
	return n, err
}

// Dir returns the state directory backing this store.
func (s *Store) Dir() string { return s.dir }

// Schema returns the on-disk layout version this database carries.
func (s *Store) Schema() int { return s.schema }

// HostID returns this machine's stable opaque id.
func (s *Store) HostID() string { return s.hostID }

// Hostname returns the OS hostname captured at Open.
func (s *Store) Hostname() string { return s.hostname }

// Append stores a single record and returns it in canonical form (with Seq,
// HostID, Hostname, and ID filled in). It is a thin wrapper over the batch
// path. If the record's ID is already present the stored copy is returned
// and nothing is added.
func (s *Store) Append(r rec.Record) (rec.Record, error) {
	var out rec.Record
	err := s.db.Update(func(tx *bbolt.Tx) error {
		stored, _, err := s.appendOne(tx, r)
		out = stored
		return err
	})
	if err != nil {
		return rec.Record{}, err
	}
	return out, nil
}

// AppendBatch stores rs in one transaction and returns how many were newly
// added (records whose ID already exists are skipped for idempotency).
func (s *Store) AppendBatch(rs []rec.Record) (int, error) {
	added := 0
	err := s.db.Update(func(tx *bbolt.Tx) error {
		for _, r := range rs {
			_, ok, err := s.appendOne(tx, r)
			if err != nil {
				return err
			}
			if ok {
				added++
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return added, nil
}

// appendOne applies one record within tx. It returns the stored record in
// canonical form and whether it was newly added. A record whose ID is already
// indexed is a no-op that returns the existing stored copy.
func (s *Store) appendOne(tx *bbolt.Tx, r rec.Record) (rec.Record, bool, error) {
	hb := tx.Bucket(bucketHistory)
	ib := tx.Bucket(bucketIDs)

	if r.ID != "" {
		if key := ib.Get([]byte(r.ID)); key != nil {
			var existing rec.Record
			if v := hb.Get(key); v != nil {
				if err := json.Unmarshal(v, &existing); err != nil {
					return rec.Record{}, false, err
				}
			}
			return existing, false, nil
		}
	}

	seq, err := hb.NextSequence()
	if err != nil {
		return rec.Record{}, false, err
	}
	r.Seq = seq
	r.HostID = s.hostID
	r.Hostname = s.hostname
	if r.ID == "" {
		r.ID = rec.NewID()
	}

	// A delete tombstone is appended to the stream like any record (sync
	// replays it), and additionally applies DeletedMs to its target if the
	// target lives in this store.
	if r.Type == rec.TypeDelete && r.TargetID != "" {
		if tkey := ib.Get([]byte(r.TargetID)); tkey != nil {
			if v := hb.Get(tkey); v != nil {
				var target rec.Record
				if err := json.Unmarshal(v, &target); err != nil {
					return rec.Record{}, false, err
				}
				ts := r.StartMs
				if ts == 0 {
					ts = time.Now().UnixMilli()
				}
				target.DeletedMs = ts
				nv, err := json.Marshal(target)
				if err != nil {
					return rec.Record{}, false, err
				}
				if err := hb.Put(tkey, nv); err != nil {
					return rec.Record{}, false, err
				}
			}
		}
	}

	key := seqKey(seq)
	val, err := json.Marshal(r)
	if err != nil {
		return rec.Record{}, false, err
	}
	if err := hb.Put(key, val); err != nil {
		return rec.Record{}, false, err
	}
	if err := ib.Put([]byte(r.ID), key); err != nil {
		return rec.Record{}, false, err
	}
	return r, true, nil
}

// IngestSpool drains the spool into the store in one batch, returning how many
// records were newly added. Records are fsynced in the spool before Drain
// removes them; the subsequent AppendBatch is idempotent by ID, so a crash in
// the narrow window between drain and commit at most reprocesses harmlessly.
func (s *Store) IngestSpool() (int, error) {
	var drained []rec.Record
	_, err := spool.Drain(config.SpoolDir(s.dir), func(r rec.Record) error {
		drained = append(drained, r)
		return nil
	})
	if err != nil {
		return 0, err
	}
	if len(drained) == 0 {
		return 0, nil
	}
	return s.AppendBatch(drained)
}

// IsCommand reports whether a raw-stream record belongs in the searchable
// command corpus. Tombstones, tag ops, and prompt records are all part of the
// stream (sync replays them) but none of them is a command, and a record that
// has been tombstoned locally is no longer live.
func IsCommand(r rec.Record) bool {
	switch r.Type {
	case rec.TypeDelete, rec.TypeTag, rec.TypePrompt:
		return false
	}
	return r.DeletedMs == 0
}

// All returns the live history in ascending seq order, excluding tombstone,
// tag, and prompt records and anything marked deleted. This is the search
// corpus.
func (s *Store) All() ([]rec.Record, error) {
	var out []rec.Record
	err := s.db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketHistory).Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var r rec.Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			if !IsCommand(r) {
				continue
			}
			out = append(out, r)
		}
		return nil
	})
	return out, err
}

// Since returns the raw stream with seq strictly greater than the given seq,
// ascending, INCLUDING tombstones and deleted records (the sync push needs
// them). limit <= 0 means no limit.
func (s *Store) Since(seq uint64, limit int) ([]rec.Record, error) {
	var out []rec.Record
	err := s.db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketHistory).Cursor()
		for k, v := c.Seek(seqKey(seq + 1)); k != nil; k, v = c.Next() {
			var r rec.Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			out = append(out, r)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
		return nil
	})
	return out, err
}

// Count reports how many rows All would return. It is O(n): the whole stream
// is scanned so tombstones and deleted records can be excluded.
func (s *Store) Count() (int, error) {
	n := 0
	err := s.db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketHistory).Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var r rec.Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			if !IsCommand(r) {
				continue
			}
			n++
		}
		return nil
	})
	return n, err
}

// TagRecords returns every TypeTag record in the stream, ascending. The daemon
// scans these once at startup to seed its tag index, since tag records are not
// part of the command corpus (and so not in the warm snapshot).
func (s *Store) TagRecords() ([]rec.Record, error) { return s.recordsOfType(rec.TypeTag) }

// PromptRecords returns every TypePrompt record in the stream, ascending. Like
// tag records these live outside the command corpus, so the daemon seeds its
// prompt index from its own scan at startup.
func (s *Store) PromptRecords() ([]rec.Record, error) { return s.recordsOfType(rec.TypePrompt) }

// recordsOfType scans the raw stream for one record type, ascending.
func (s *Store) recordsOfType(typ string) ([]rec.Record, error) {
	var out []rec.Record
	err := s.db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketHistory).Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var r rec.Record
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			if r.Type == typ {
				out = append(out, r)
			}
		}
		return nil
	})
	return out, err
}

// LastSeq returns the highest seq assigned in the raw stream, or 0 if empty.
func (s *Store) LastSeq() (uint64, error) {
	var seq uint64
	err := s.db.View(func(tx *bbolt.Tx) error {
		seq = tx.Bucket(bucketHistory).Sequence()
		return nil
	})
	return seq, err
}

// MarkDeleted stamps DeletedMs on the record with the given ID via the ids
// index. ts of 0 means now. A missing ID is an error.
func (s *Store) MarkDeleted(id string, ts int64) error {
	if ts == 0 {
		ts = time.Now().UnixMilli()
	}
	return s.db.Update(func(tx *bbolt.Tx) error {
		hb := tx.Bucket(bucketHistory)
		ib := tx.Bucket(bucketIDs)
		key := ib.Get([]byte(id))
		if key == nil {
			return fmt.Errorf("store: record %q not found", id)
		}
		v := hb.Get(key)
		if v == nil {
			return fmt.Errorf("store: record %q indexed but missing from history", id)
		}
		var r rec.Record
		if err := json.Unmarshal(v, &r); err != nil {
			return err
		}
		r.DeletedMs = ts
		nv, err := json.Marshal(r)
		if err != nil {
			return err
		}
		return hb.Put(key, nv)
	})
}

// Meta returns the meta value for key, or "" if absent.
func (s *Store) Meta(key string) (string, error) {
	var val string
	err := s.db.View(func(tx *bbolt.Tx) error {
		if v := tx.Bucket(bucketMeta).Get([]byte(key)); v != nil {
			val = string(v)
		}
		return nil
	})
	return val, err
}

// SetMeta stores a meta key/value.
func (s *Store) SetMeta(key, value string) error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketMeta).Put([]byte(key), []byte(value))
	})
}

func seqKey(seq uint64) []byte {
	var k [8]byte
	binary.BigEndian.PutUint64(k[:], seq)
	return k[:]
}
