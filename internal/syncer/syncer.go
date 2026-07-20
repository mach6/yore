package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"yore/internal/cryptobox"
	"yore/internal/rec"
	"yore/internal/store"
	"yore/internal/wire"
)

// metaLastUploadedSeq is the store meta key holding the push watermark: the
// highest local seq already uploaded. It persists across restarts so records
// are never re-pushed. Pull cursors, by contrast, are RAM-only (see PullOthers).
const metaLastUploadedSeq = "last_uploaded_seq"

// dekPageLimit bounds each DEK-list page during a wrap sweep.
const dekPageLimit = 1000

// pushBatchLimit is the maximum records per push (the server's own cap).
const pushBatchLimit = 1000

// dekEntry is a cached epoch data key: its keyID and the unwrapped 32-byte key.
type dekEntry struct {
	keyID string
	dek   [32]byte
}

// Syncer is the sync engine for one machine. It owns all cryptobox usage and
// holds unwrapped key material (the History Key and epoch DEKs) in RAM only —
// never on disk. Its identity is the device key plus the store's stable hostID,
// which doubles as this machine's device ID in the server's device registry.
//
// The zero value is unusable; construct one with New. A Syncer is safe for
// concurrent use; a mutex guards the in-RAM key caches.
type Syncer struct {
	st    *store.Store
	http  *HTTPClient
	dev   cryptobox.DeviceKey
	epoch time.Duration

	// deviceID is this machine's identifier in the server's device registry.
	// It equals the store's hostID, so it is stable across restarts without
	// any extra persistence and is distinct per machine.
	deviceID string
	hostID   string

	mu sync.Mutex // guards everything below

	hkResolved bool
	hk         [32]byte
	hkVersion  int

	deksByEpoch map[int64]dekEntry      // push side: epoch start -> DEK
	deksByKeyID map[string][32]byte     // pull side: keyID -> unwrapped DEK
	dekWraps    map[string]wire.DEKWrap // pull side: keyID -> wrap metadata
}

// New builds a Syncer over the given local store, transport, device key, and
// epoch width. The epoch width buckets records into DEKs (one DEK per epoch);
// callers pass config.KeyEpochD().
func New(st *store.Store, http *HTTPClient, dev cryptobox.DeviceKey, epoch time.Duration) *Syncer {
	id := st.HostID()
	// Sign this device's mutating requests with its Ed25519 key.
	http.SetSigner(id, dev.Sign)
	return &Syncer{
		st:          st,
		http:        http,
		dev:         dev,
		epoch:       epoch,
		deviceID:    id,
		hostID:      id,
		deksByEpoch: make(map[int64]dekEntry),
		deksByKeyID: make(map[string][32]byte),
		dekWraps:    make(map[string]wire.DEKWrap),
	}
}

// DeviceID returns this machine's device/host identifier.
func (s *Syncer) DeviceID() string { return s.deviceID }

// payload is the plaintext that gets sealed into a record blob. Every
// meaningful field of the record travels encrypted, including the hostname.
// Exit and DurMs are pointers so nil (unknown) is preserved distinctly from a
// zero value across the round trip.
type payload struct {
	V        int    `json:"v"`
	ID       string `json:"id"`
	HostID   string `json:"host_id"`
	Hostname string `json:"hostname,omitempty"`
	Session  string `json:"session,omitempty"`
	Cmd      string `json:"cmd,omitempty"`
	Cwd      string `json:"cwd,omitempty"`
	Exit     *int   `json:"exit,omitempty"`
	DurMs    *int64 `json:"dur_ms,omitempty"`
	StartMs  int64  `json:"start_ms,omitempty"`
	Tag      string `json:"tag,omitempty"`
	Type     string `json:"type,omitempty"`
	TargetID string `json:"target_id,omitempty"`
}

// marshalPayload renders a record's meaningful fields as the sealed plaintext.
func marshalPayload(r rec.Record) ([]byte, error) {
	return json.Marshal(payload{
		V:        1,
		ID:       r.ID,
		HostID:   r.HostID,
		Hostname: r.Hostname,
		Session:  r.Session,
		Cmd:      r.Cmd,
		Cwd:      r.Cwd,
		Exit:     r.Exit,
		DurMs:    r.DurMs,
		StartMs:  r.StartMs,
		Tag:      r.Tag,
		Type:     r.Type,
		TargetID: r.TargetID,
	})
}

// recordFromPayload reconstructs a rec.Record from decrypted payload bytes plus
// the authenticated stream metadata (hostID, seq, keyID) that rode in the wire
// record rather than the ciphertext.
func recordFromPayload(pt []byte, hostID string, seq uint64, keyID string) (rec.Record, error) {
	var p payload
	if err := json.Unmarshal(pt, &p); err != nil {
		return rec.Record{}, fmt.Errorf("syncer: decode payload: %w", err)
	}
	return rec.Record{
		ID:       p.ID,
		Type:     p.Type,
		TargetID: p.TargetID,
		HostID:   hostID,
		Hostname: p.Hostname,
		Seq:      seq,
		Session:  p.Session,
		Cmd:      p.Cmd,
		Cwd:      p.Cwd,
		Exit:     p.Exit,
		DurMs:    p.DurMs,
		StartMs:  p.StartMs,
		Tag:      p.Tag,
		KeyID:    keyID,
	}, nil
}

// resolveHK fetches this device's HK wrap and unwraps it into RAM, caching the
// result (and the HK version) for the lifetime of the Syncer. It errors if the
// server has no wrap for us — meaning this device is not (or no longer) an
// active member of the group.
func (s *Syncer) resolveHK(ctx context.Context) ([32]byte, int, error) {
	s.mu.Lock()
	if s.hkResolved {
		hk, ver := s.hk, s.hkVersion
		s.mu.Unlock()
		return hk, ver, nil
	}
	s.mu.Unlock()

	wrap, found, err := s.http.GetHKWrap(ctx, s.deviceID)
	if err != nil {
		return [32]byte{}, 0, err
	}
	if !found {
		return [32]byte{}, 0, fmt.Errorf("syncer: no history key for this device (not activated or revoked)")
	}
	hk, err := cryptobox.UnwrapHK(wrap.Blob, s.dev)
	if err != nil {
		return [32]byte{}, 0, fmt.Errorf("syncer: unwrap history key: %w", err)
	}

	s.mu.Lock()
	s.hk = hk
	s.hkVersion = wrap.HKVersion
	s.hkResolved = true
	s.mu.Unlock()
	return hk, wrap.HKVersion, nil
}

// setHK replaces the cached HK and version and clears the now-stale DEK-wrap
// cache. The unwrapped DEK plaintexts stay valid across a rotation (rotation
// re-wraps DEKs but never changes their plaintext), so deksByKeyID and
// deksByEpoch are kept.
func (s *Syncer) setHK(hk [32]byte, version int) {
	s.mu.Lock()
	s.hk = hk
	s.hkVersion = version
	s.hkResolved = true
	s.dekWraps = make(map[string]wire.DEKWrap)
	s.mu.Unlock()
}

// Push encrypts and uploads every local record with seq greater than the
// persisted watermark, in ascending batches of at most pushBatchLimit. After
// each acknowledged batch it advances (and persists) the watermark, so a failed
// batch leaves the watermark untouched and is retried on the next call. It
// returns the total number of records uploaded.
func (s *Syncer) Push(ctx context.Context) (int, error) {
	hk, hkVersion, err := s.resolveHK(ctx)
	if err != nil {
		return 0, err
	}

	watermark, err := s.readWatermark()
	if err != nil {
		return 0, err
	}

	pushed := 0
	for {
		batch, err := s.st.Since(watermark, pushBatchLimit)
		if err != nil {
			return pushed, fmt.Errorf("syncer: read local records: %w", err)
		}
		if len(batch) == 0 {
			return pushed, nil
		}

		records := make([]wire.PushRecord, 0, len(batch))
		for _, r := range batch {
			epoch := cryptobox.EpochStart(time.UnixMilli(r.StartMs), s.epoch)
			entry, err := s.ensurePushDEK(ctx, hk, hkVersion, epoch)
			if err != nil {
				return pushed, err
			}
			aad := cryptobox.RecordAAD{
				RecordID: r.ID,
				HostID:   r.HostID,
				Seq:      r.Seq,
				KeyID:    entry.keyID,
			}
			pt, err := marshalPayload(r)
			if err != nil {
				return pushed, fmt.Errorf("syncer: marshal payload: %w", err)
			}
			blob, err := cryptobox.SealRecord(pt, entry.dek, aad)
			if err != nil {
				return pushed, fmt.Errorf("syncer: seal record: %w", err)
			}
			records = append(records, wire.PushRecord{
				Seq:   r.Seq,
				ID:    r.ID,
				KeyID: entry.keyID,
				Blob:  blob,
			})
		}

		if _, err := s.http.PushRecords(ctx, wire.PushReq{HostID: s.hostID, Records: records}); err != nil {
			// Do not advance the watermark on a failed batch.
			return pushed, err
		}

		pushed += len(records)
		watermark = batch[len(batch)-1].Seq
		if err := s.writeWatermark(watermark); err != nil {
			return pushed, err
		}
	}
}

// ensurePushDEK returns the DEK for an epoch, minting and uploading a fresh one
// the first time an epoch is seen this process. Across restarts a new DEK may
// be minted for an already-populated epoch (a few extra tiny wraps); that is
// acceptable and every such DEK still decrypts its own records.
func (s *Syncer) ensurePushDEK(ctx context.Context, hk [32]byte, hkVersion int, epoch int64) (dekEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if e, ok := s.deksByEpoch[epoch]; ok {
		return e, nil
	}

	keyID, dek, err := cryptobox.NewDEK()
	if err != nil {
		return dekEntry{}, fmt.Errorf("syncer: new DEK: %w", err)
	}
	blob, err := cryptobox.WrapDEK(dek, hk, keyID, s.deviceID, epoch, hkVersion)
	if err != nil {
		return dekEntry{}, fmt.Errorf("syncer: wrap DEK: %w", err)
	}
	wrap := wire.DEKWrap{
		KeyID:     keyID,
		DeviceID:  s.deviceID,
		Epoch:     epoch,
		Blob:      blob,
		HKVersion: hkVersion,
	}
	if _, err := s.http.UploadDEKWraps(ctx, []wire.DEKWrap{wrap}); err != nil {
		return dekEntry{}, err
	}

	entry := dekEntry{keyID: keyID, dek: dek}
	s.deksByEpoch[epoch] = entry
	s.deksByKeyID[keyID] = dek
	s.dekWraps[keyID] = wrap
	return entry, nil
}

// PullOthers pulls every remote host stream incrementally from the passed-in
// cursors, decrypts every record, and returns the decrypted records together
// with the advanced cursor set. The self host is skipped (the local store
// already holds those records). Remote records are NEVER persisted locally: the
// caller (the daemon) keeps the returned records in memory for its lifetime, so
// a fresh daemon re-pulls from empty cursors.
//
// Decryption failure is FATAL: it signals tampering or a key mismatch and is
// returned as an error, never silently skipped. The passed-in cursors map is
// not mutated; a fresh advanced map is returned.
func (s *Syncer) PullOthers(ctx context.Context, cursors map[string]uint64) ([]rec.Record, map[string]uint64, error) {
	hk, _, err := s.resolveHK(ctx)
	if err != nil {
		return nil, nil, err
	}

	newCursors := make(map[string]uint64, len(cursors))
	for k, v := range cursors {
		newCursors[k] = v
	}

	hosts, err := s.http.Hosts(ctx)
	if err != nil {
		return nil, nil, err
	}

	var out []rec.Record
	for _, h := range hosts {
		if h.HostID == s.hostID {
			continue // our own stream is already local
		}
		after := newCursors[h.HostID]
		for {
			resp, err := s.http.PullRecords(ctx, h.HostID, after, pushBatchLimit)
			if err != nil {
				return nil, nil, err
			}
			for _, pr := range resp.Records {
				r, err := s.openRecord(ctx, hk, h.HostID, pr)
				if err != nil {
					return nil, nil, err
				}
				out = append(out, r)
				after = pr.Seq
			}
			newCursors[h.HostID] = after
			if resp.NextAfter == nil {
				break
			}
		}
	}
	return out, newCursors, nil
}

// openRecord decrypts one sealed pull record into a rec.Record, reconstructing
// the authenticated stream fields (hostID, seq, keyID) from the wire record.
// Any authentication failure is returned as an error (fatal to the pull).
func (s *Syncer) openRecord(ctx context.Context, hk [32]byte, hostID string, pr wire.PullRecord) (rec.Record, error) {
	dek, err := s.dekForKeyID(ctx, hk, pr.KeyID)
	if err != nil {
		return rec.Record{}, err
	}
	aad := cryptobox.RecordAAD{
		RecordID: pr.ID,
		HostID:   hostID,
		Seq:      pr.Seq,
		KeyID:    pr.KeyID,
	}
	pt, err := cryptobox.OpenRecord(pr.Blob, dek, aad)
	if err != nil {
		return rec.Record{}, fmt.Errorf("syncer: open record %s (host %s seq %d): %w", pr.ID, hostID, pr.Seq, err)
	}
	return recordFromPayload(pt, hostID, pr.Seq, pr.KeyID)
}

// dekForKeyID returns the unwrapped DEK for a keyID, unwrapping it under the HK
// on first use and caching it. On a cache miss it sweeps the server's DEK-wrap
// set to locate the wrap metadata. An unwrap failure for the requested key is
// returned as an error.
func (s *Syncer) dekForKeyID(ctx context.Context, hk [32]byte, keyID string) ([32]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if dek, ok := s.deksByKeyID[keyID]; ok {
		return dek, nil
	}
	if _, ok := s.dekWraps[keyID]; !ok {
		if err := s.refreshDEKWrapsLocked(ctx); err != nil {
			return [32]byte{}, err
		}
	}
	wrap, ok := s.dekWraps[keyID]
	if !ok {
		return [32]byte{}, fmt.Errorf("syncer: no DEK wrap for key %s", keyID)
	}
	dek, err := cryptobox.UnwrapDEK(wrap.Blob, hk, wrap.KeyID, wrap.DeviceID, wrap.Epoch, wrap.HKVersion)
	if err != nil {
		return [32]byte{}, fmt.Errorf("syncer: unwrap DEK %s: %w", keyID, err)
	}
	s.deksByKeyID[keyID] = dek
	return dek, nil
}

// refreshDEKWrapsLocked pages the entire DEK-wrap set into the wrap cache. The
// caller must hold s.mu.
func (s *Syncer) refreshDEKWrapsLocked(ctx context.Context) error {
	cursor := ""
	for {
		resp, err := s.http.ListDEKWraps(ctx, cursor, dekPageLimit)
		if err != nil {
			return err
		}
		for _, w := range resp.Wraps {
			if _, ok := s.dekWraps[w.KeyID]; !ok {
				s.dekWraps[w.KeyID] = w
			}
		}
		if resp.NextCursor == "" {
			return nil
		}
		cursor = resp.NextCursor
	}
}

// readWatermark loads the persisted push watermark (0 if unset).
func (s *Syncer) readWatermark() (uint64, error) {
	v, err := s.st.Meta(metaLastUploadedSeq)
	if err != nil {
		return 0, fmt.Errorf("syncer: read watermark: %w", err)
	}
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("syncer: parse watermark %q: %w", v, err)
	}
	return n, nil
}

// writeWatermark persists the push watermark.
func (s *Syncer) writeWatermark(seq uint64) error {
	if err := s.st.SetMeta(metaLastUploadedSeq, strconv.FormatUint(seq, 10)); err != nil {
		return fmt.Errorf("syncer: persist watermark: %w", err)
	}
	return nil
}
