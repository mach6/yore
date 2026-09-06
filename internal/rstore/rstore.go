// Package rstore is the daemon's on-disk cache of OTHER machines' history. It
// holds exactly what the sync server holds (sealed record blobs this package
// cannot read and has no key for) so yore's rule that other hosts' PLAINTEXT
// never touches this machine's disk is untouched: decryption happens in the
// daemon's RAM, from here, and the result is never written back. What it buys
// is bounded, incremental startup. Pull cursors used to live only in RAM, so
// every daemon lifetime re-downloaded and re-decrypted every other machine's
// entire archive from seq 0; work proportional to all history ever recorded,
// repeated several times a day once the idle timeout recycles the process. Here
// the cursor is persisted next to the ciphertext it describes, so a restart
// fetches only what is genuinely new, and Keep caps how much of each host's
// tail is retained at all. The cache is DERIVED data: deleting the file costs
// one full re-pull and nothing else. It is stored separately from data.db
// precisely because data.db is defined as holding only this host's own history.
package rstore

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.etcd.io/bbolt"
	bolterrors "go.etcd.io/bbolt/errors"

	"yore/internal/config"
	"yore/internal/wire"
)

// ErrLocked is returned by Open when another process already holds the cache.
var ErrLocked = errors.New("rstore: locked by another process")

// streamPrefix names one remote host's bucket: streamPrefix+hostID, mapping
// seq (8-byte big-endian) -> the sealed record as JSON.
const streamPrefix = "stream:"

// bucketCursors maps hostID -> the highest seq pulled for it (8-byte BE). It is
// the resume point, persisted so a restart does not re-fetch the archive.
var bucketCursors = []byte("cursors")

// bucketCounts maps hostID -> how many records its stream holds (8-byte BE).
// It is maintained rather than measured because bbolt's Bucket.Stats reports
// the B-tree as last written, NOT including puts still pending in the current
// transaction, so trimming off Stats inside the same Update that appended
// would work from a count several records stale and under-trim.
var bucketCounts = []byte("counts")

// Path is where the remote ciphertext cache lives inside the state dir.
func Path(dir string) string { return filepath.Join(dir, "remote.db") }

// Store is an open handle to the remote ciphertext cache.
type Store struct {
	db  *bbolt.DB
	dir string
}

// Open opens (creating if needed) the cache under the state dir. Like the main
// store it takes an exclusive lock; a competing opener is reported rather than
// blocking.
func Open(dir string) (*Store, error) {
	if err := config.EnsureDir(dir); err != nil {
		return nil, err
	}
	db, err := bbolt.Open(Path(dir), 0o600, &bbolt.Options{Timeout: 200 * time.Millisecond})
	if err != nil {
		if errors.Is(err, bolterrors.ErrTimeout) {
			return nil, ErrLocked
		}
		return nil, err
	}
	if err := db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bucketCursors, bucketCounts} {
			if _, cerr := tx.CreateBucketIfNotExists(name); cerr != nil {
				return cerr
			}
		}
		return nil
	}); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, dir: dir}, nil
}

// Close releases the cache and its lock.
func (s *Store) Close() error { return s.db.Close() }

// Hosts returns every host id with cached records.
func (s *Store) Hosts() ([]string, error) {
	var out []string
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
			if id, ok := strings.CutPrefix(string(name), streamPrefix); ok {
				out = append(out, id)
			}
			return nil
		})
	})
	return out, err
}

// Cursors returns the persisted pull position for every known host.
func (s *Store) Cursors() (map[string]uint64, error) {
	out := map[string]uint64{}
	err := s.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketCursors).ForEach(func(k, v []byte) error {
			out[string(k)] = beUint64(v)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Records returns one host's cached sealed records in ascending seq order.
func (s *Store) Records(hostID string) ([]wire.PullRecord, error) {
	var out []wire.PullRecord
	err := s.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(streamPrefix + hostID))
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var pr wire.PullRecord
			if err := json.Unmarshal(v, &pr); err != nil {
				return fmt.Errorf("rstore: decode cached record (host %s seq %d): %w", hostID, beUint64(k), err)
			}
			out = append(out, pr)
		}
		return nil
	})
	return out, err
}

// Append caches a batch of sealed records for one host, advances that host's
// cursor to `cursor`, and trims the stream to the newest `keep` records
// (keep <= 0 retains everything). Records already present are overwritten
// harmlessly: the server's stream is append-only and immutable per seq. It
// returns how many records were trimmed, so the caller can mirror the
// eviction in its RAM cache.
func (s *Store) Append(hostID string, recs []wire.PullRecord, cursor uint64, keep int) (trimmed int, err error) {
	if hostID == "" {
		return 0, errors.New("rstore: empty host id")
	}
	err = s.db.Update(func(tx *bbolt.Tx) error {
		b, berr := tx.CreateBucketIfNotExists([]byte(streamPrefix + hostID))
		if berr != nil {
			return berr
		}
		count := loadCount(tx, hostID)
		for i := range recs {
			val, merr := json.Marshal(recs[i])
			if merr != nil {
				return merr
			}
			key := seqKey(recs[i].Seq)
			fresh := b.Get(key) == nil // re-appending a cached seq must not inflate the count
			if perr := b.Put(key, val); perr != nil {
				return perr
			}
			if fresh {
				count++
			}
		}

		var terr error
		trimmed, terr = trim(b, count, keep)
		if terr != nil {
			return terr
		}
		count -= trimmed

		if perr := tx.Bucket(bucketCounts).Put([]byte(hostID), seqKey(uint64(count))); perr != nil {
			return perr
		}
		return tx.Bucket(bucketCursors).Put([]byte(hostID), seqKey(cursor))
	})
	if err != nil {
		return 0, err
	}
	return trimmed, nil
}

// loadCount returns a stream's record count. Append is the only writer and it
// records the count in the same transaction as the records themselves, so a
// stream with no count entry is a stream with no records.
func loadCount(tx *bbolt.Tx, hostID string) int {
	return int(beUint64(tx.Bucket(bucketCounts).Get([]byte(hostID))))
}

// trim deletes the oldest records until at most keep remain, given the stream's
// current count, and reports how many it removed.
func trim(b *bbolt.Bucket, count, keep int) (int, error) {
	if keep <= 0 || count <= keep {
		return 0, nil
	}
	excess := count - keep
	c := b.Cursor()
	deleted := 0
	// Re-seek to First on every iteration rather than advancing with Next: after
	// Cursor.Delete the cursor still points at the removed position, so a Next
	// from there steps over the record that took its place and only every other
	// one would go.
	for deleted < excess {
		if k, _ := c.First(); k == nil {
			break
		}
		if err := c.Delete(); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// Reset drops every cached stream and cursor. The daemon calls it when the sync
// server or group changes: cached ciphertext belongs to the old group's key
// hierarchy and must never be mixed with the new one's.
func (s *Store) Reset() error {
	return s.db.Update(func(tx *bbolt.Tx) error {
		var names [][]byte
		if err := tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
			names = append(names, append([]byte(nil), name...))
			return nil
		}); err != nil {
			return err
		}
		for _, name := range names {
			if err := tx.DeleteBucket(name); err != nil {
				return err
			}
		}
		for _, name := range [][]byte{bucketCursors, bucketCounts} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		return nil
	})
}

// Remove deletes the cache file entirely. Used when the daemon cannot open it:
// a corrupt cache must cost a re-pull, never a daemon that refuses to start.
func Remove(dir string) error {
	err := os.Remove(Path(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

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
