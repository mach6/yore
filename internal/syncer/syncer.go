package syncer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

// maxPushBytes bounds the encoded size of one push body, comfortably under the
// server's 10 MiB request cap. A record count alone is not a bound: a thousand
// records carrying long prompts or heredocs is megabytes, and a body over the
// server's limit fails EVERY retry — the watermark never advances and sync
// wedges permanently. Size is what the server actually limits, so size is what
// the client batches on.
const maxPushBytes = 8 << 20

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

	// syncPrompts governs whether prompt records leave this machine. When false
	// they stay in the local store and are never sealed or uploaded, so agent
	// prompts remain readable here and nowhere else. Commands still sync, and
	// still carry their PromptID — on another machine that id simply resolves to
	// no text.
	syncPrompts bool

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
// callers pass config.KeyEpochD(). Prompt records are synced by default; see
// SetSyncPrompts.
func New(st *store.Store, http *HTTPClient, dev cryptobox.DeviceKey, epoch time.Duration) *Syncer {
	id := st.HostID()
	// Sign this device's mutating requests with its Ed25519 key.
	http.SetSigner(id, dev.Sign)
	return &Syncer{
		st:          st,
		http:        http,
		dev:         dev,
		epoch:       epoch,
		syncPrompts: true,
		deviceID:    id,
		hostID:      id,
		deksByEpoch: make(map[int64]dekEntry),
		deksByKeyID: make(map[string][32]byte),
		dekWraps:    make(map[string]wire.DEKWrap),
	}
}

// SetSyncPrompts chooses whether agent prompt records are uploaded (callers
// pass config.SyncPrompts). Turning it off is not retroactive: prompts already
// pushed stay on the server, because the watermark has passed them.
func (s *Syncer) SetSyncPrompts(v bool) {
	s.mu.Lock()
	s.syncPrompts = v
	s.mu.Unlock()
}

// promptsSynced reports whether prompt text may leave this machine.
func (s *Syncer) promptsSynced() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncPrompts
}

// pushable reports whether a local record should be uploaded at all.
func (s *Syncer) pushable(r rec.Record) bool {
	return r.Type != rec.TypePrompt || s.promptsSynced()
}

// DeviceID returns this machine's device/host identifier.
func (s *Syncer) DeviceID() string { return s.deviceID }

// payload is the plaintext that gets sealed into a record blob. Every
// meaningful field of the record travels encrypted, including the hostname.
// Exit and DurMs are pointers so nil (unknown) is preserved distinctly from a
// zero value across the round trip.
type payload struct {
	// V is the shape of what follows. 2 moved the executor off the "tag" key and
	// onto its own; 1 is pre-release and not read anywhere.
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
	Executor string `json:"executor,omitempty"`
	Type     string `json:"type,omitempty"`
	TargetID string `json:"target_id,omitempty"`
	PromptID string `json:"prompt_id,omitempty"`
	Prompt   string `json:"prompt,omitempty"`
	TagName  string `json:"tag_name,omitempty"`
	TagDesc  string `json:"tag_desc,omitempty"`
	TagOp    string `json:"tag_op,omitempty"`
}

// marshalPayload renders a record's meaningful fields as the sealed plaintext.
func marshalPayload(r rec.Record) ([]byte, error) {
	return json.Marshal(payload{
		V:        2,
		ID:       r.ID,
		HostID:   r.HostID,
		Hostname: r.Hostname,
		Session:  r.Session,
		Cmd:      r.Cmd,
		Cwd:      r.Cwd,
		Exit:     r.Exit,
		DurMs:    r.DurMs,
		StartMs:  r.StartMs,
		Executor: r.Executor,
		Type:     r.Type,
		TargetID: r.TargetID,
		PromptID: r.PromptID,
		Prompt:   r.Prompt,
		TagName:  r.TagName,
		TagDesc:  r.TagDesc,
		TagOp:    r.TagOp,
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
		Executor: p.Executor,
		PromptID: p.PromptID,
		Prompt:   p.Prompt,
		TagName:  p.TagName,
		TagDesc:  p.TagDesc,
		TagOp:    p.TagOp,
		KeyID:    keyID,
	}, nil
}

// resolveHK fetches this device's HK wrap and unwraps it into RAM, caching the
// result (and the HK version) for the lifetime of the Syncer. It errors if the
// server has no wrap for us — meaning this device is not (or no longer) an
// active member of the group.
func (s *Syncer) resolveHK(ctx context.Context) (key [32]byte, version int, err error) {
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
// persisted watermark, in ascending batches bounded by BOTH pushBatchLimit
// records and maxPushBytes of encoded body. After each acknowledged batch it
// advances (and persists) the watermark, so a failed batch leaves the watermark
// untouched and is retried on the next call. It returns the total number of
// records uploaded.
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

		// Seal a size-bounded prefix of the batch. srcIdx[i] is the index in
		// batch that records[i] came from, so a short send can still advance the
		// watermark exactly as far as the server accepted.
		records := make([]wire.PushRecord, 0, len(batch))
		srcIdx := make([]int, 0, len(batch))
		size := 0
		consumed := 0
		for i, r := range batch {
			if !s.pushable(r) {
				consumed = i + 1 // skipped, but the stream position still passes it
				continue
			}
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
			pr := wire.PushRecord{Seq: r.Seq, ID: r.ID, KeyID: entry.keyID, Blob: blob}
			// Stop before exceeding the cap — but never emit an empty batch, so a
			// single oversized record is still attempted (and fails loudly) rather
			// than silently stalling the stream forever.
			n := pushRecordSize(pr)
			if len(records) > 0 && size+n > maxPushBytes {
				break
			}
			size += n
			records = append(records, pr)
			srcIdx = append(srcIdx, i)
			consumed = i + 1
		}
		if consumed == 0 {
			return pushed, fmt.Errorf("syncer: push made no progress at seq %d", batch[0].Seq)
		}

		if len(records) == 0 {
			// Everything in this window was filtered out (prompt records, with
			// prompt sync off). Bank the position and carry on.
			watermark = batch[consumed-1].Seq
			if err := s.writeWatermark(watermark); err != nil {
				return pushed, err
			}
			continue
		}

		sent, err := s.pushWithBackoff(ctx, records)
		if err != nil {
			return pushed, err // watermark untouched: the whole batch retries
		}
		pushed += sent
		if sent == len(records) {
			watermark = batch[consumed-1].Seq
		} else {
			watermark = batch[srcIdx[sent-1]].Seq
		}
		if err := s.writeWatermark(watermark); err != nil {
			return pushed, err
		}
	}
}

// pushWithBackoff uploads records, halving the batch and retrying whenever the
// server rejects it in a way a smaller body would fix. It returns how many
// leading records were accepted.
//
// The size estimate this backs up is only an estimate — a server configured
// with a tighter limit, or a proxy in between, can still refuse a body we
// thought was fine. Halving converges in a few round trips and, crucially,
// terminates: once a single record is rejected the error is real and is
// returned, instead of retrying an impossible batch forever.
func (s *Syncer) pushWithBackoff(ctx context.Context, records []wire.PushRecord) (int, error) {
	n := len(records)
	for {
		_, err := s.http.PushRecords(ctx, wire.PushReq{HostID: s.hostID, Records: records[:n]})
		if err == nil {
			return n, nil
		}
		if n <= 1 || !retryableSmaller(err) {
			return 0, err
		}
		n /= 2
	}
}

// retryableSmaller reports whether an error is one a smaller batch might avoid:
// an explicit 413, or a 400 — which is what a body cut off by the server's
// MaxBytesReader looks like once JSON decoding fails on the truncated stream.
// A genuinely malformed request also 400s, but it 400s at every size too, so
// the halving loop terminates on it rather than masking it.
func retryableSmaller(err error) bool {
	var ae *APIError
	if !errors.As(err, &ae) {
		return false
	}
	return ae.Status == http.StatusRequestEntityTooLarge || ae.Status == http.StatusBadRequest
}

// pushRecordSize estimates one record's JSON footprint inside a PushReq: the
// base64-expanded blob plus the field names, quoting, and separators around it.
func pushRecordSize(pr wire.PushRecord) int {
	const scaffolding = 64 // {"seq":N,"id":"","key_id":"","blob":""},
	return scaffolding + len(pr.ID) + len(pr.KeyID) + base64.StdEncoding.EncodedLen(len(pr.Blob))
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

// PullCiphertext fetches every remote host's new sealed records, starting from
// the passed-in cursors, and returns them per host together with the advanced
// cursor set. Nothing is decrypted here: the caller can cache the ciphertext
// (see internal/rstore) before spending anything on crypto, which is what lets
// a restart resume instead of re-downloading the whole archive.
//
// The self host is skipped — the local store already holds those records. The
// passed-in cursors map is not mutated; a fresh advanced map is returned.
func (s *Syncer) PullCiphertext(ctx context.Context, cursors map[string]uint64) (byHost map[string][]wire.PullRecord, advanced map[string]uint64, err error) {
	newCursors := make(map[string]uint64, len(cursors))
	for k, v := range cursors {
		newCursors[k] = v
	}

	hosts, err := s.http.Hosts(ctx)
	if err != nil {
		return nil, nil, err
	}

	out := make(map[string][]wire.PullRecord, len(hosts))
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
				out[h.HostID] = append(out[h.HostID], pr)
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

// OpenRecords decrypts one host's sealed records into plaintext history.
//
// Decryption failure is FATAL: it signals tampering or a key mismatch and is
// returned as an error, never silently skipped.
func (s *Syncer) OpenRecords(ctx context.Context, hostID string, prs []wire.PullRecord) ([]rec.Record, error) {
	if len(prs) == 0 {
		return nil, nil
	}
	hk, _, err := s.resolveHK(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]rec.Record, 0, len(prs))
	for _, pr := range prs {
		r, err := s.openRecord(ctx, hk, hostID, pr)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// PullOthers is PullCiphertext followed by OpenRecords for every host: the
// whole remote fetch in one call, for callers with no ciphertext cache to fill.
func (s *Syncer) PullOthers(ctx context.Context, cursors map[string]uint64) ([]rec.Record, map[string]uint64, error) {
	byHost, newCursors, err := s.PullCiphertext(ctx, cursors)
	if err != nil {
		return nil, nil, err
	}
	var out []rec.Record
	for hostID, prs := range byHost {
		recs, err := s.OpenRecords(ctx, hostID, prs)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, recs...)
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
