package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/mach6/yore/internal/config"
	"github.com/mach6/yore/internal/cryptobox"
	"github.com/mach6/yore/internal/match"
	"github.com/mach6/yore/internal/proto"
	"github.com/mach6/yore/internal/rec"
	"github.com/mach6/yore/internal/rstore"
	"github.com/mach6/yore/internal/secret"
	"github.com/mach6/yore/internal/store"
	"github.com/mach6/yore/internal/syncer"
	"github.com/mach6/yore/internal/wire"
)

// errSyncOff is returned by device operations when sync isn't configured.
var errSyncOff = errors.New("sync not configured (run `yore setup`)")

// errRevoked is returned once the server has refused this device as revoked.
// Retrying cannot fix it: the device has to be enrolled again.
var errRevoked = errors.New("this device has been revoked (re-enroll it with `yore enroll`)")

// listDevices returns the enrolled devices with per-device verification codes
// for pending ones (computed here so proto stays independent of wire/cryptobox).
func (s *server) listDevices() (proto.DevicesInfo, error) {
	sy := s.remote.syncer()
	if sy == nil {
		return proto.DevicesInfo{}, errSyncOff
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	devs, err := sy.Devices(ctx)
	if err != nil {
		return proto.DevicesInfo{}, err
	}
	self := sy.DeviceID()
	out := make([]proto.DeviceInfo, 0, len(devs))
	for _, d := range devs {
		di := proto.DeviceInfo{ID: d.ID, Name: d.Name, Status: d.Status, Self: d.ID == self}
		if d.Status == wire.DevicePending {
			if pub, perr := cryptobox.PublicFromBytes(d.PubKey); perr == nil {
				di.Code = syncer.VerificationCode(pub)
			}
		}
		out = append(out, di)
	}
	return proto.DevicesInfo{Devices: out}, nil
}

// deviceOp approves (approve=true) or revokes+rotates (approve=false) a device.
func (s *server) deviceOp(id string, approve bool) error {
	sy := s.remote.syncer()
	if sy == nil {
		return errSyncOff
	}
	if id == "" {
		return errors.New("empty device id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if approve {
		return sy.Approve(ctx, id)
	}
	return sy.Revoke(ctx, id)
}

// mintToken issues a single-use enrollment token for adding another machine.
func (s *server) mintToken() (proto.TokenInfo, error) {
	sy := s.remote.syncer()
	if sy == nil {
		return proto.TokenInfo{}, errSyncOff
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t, err := sy.MintToken(ctx)
	if err != nil {
		return proto.TokenInfo{}, err
	}
	return proto.TokenInfo{Token: t.Token, ExpiresMs: t.ExpiresMs}, nil
}

// listTokens reports every enrollment token the server still records, and what
// became of each. Hashes and outcomes only: the plaintexts are long gone.
func (s *server) listTokens() (proto.TokensInfo, error) {
	sy := s.remote.syncer()
	if sy == nil {
		return proto.TokensInfo{}, errSyncOff
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	toks, err := sy.Tokens(ctx)
	if err != nil {
		return proto.TokensInfo{}, err
	}
	out := make([]proto.EnrollToken, 0, len(toks))
	for _, t := range toks {
		out = append(out, proto.EnrollToken{
			ID: t.ID, State: t.State,
			CreatedMs: t.CreatedMs, ExpiresMs: t.ExpiresMs,
			ClaimedMs: t.ClaimedMs, ClaimedBy: t.ClaimedBy, RevokedMs: t.RevokedMs,
		})
	}
	return proto.TokensInfo{Tokens: out}, nil
}

// revokeToken cancels an unclaimed enrollment token. Nothing is rotated: the
// token let no one in, so there is no key anyone could already have used it to
// read (unlike revoking a device, which does rotate).
func (s *server) revokeToken(id string) error {
	sy := s.remote.syncer()
	if sy == nil {
		return errSyncOff
	}
	if id == "" {
		return errors.New("empty token id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return sy.RevokeToken(ctx, id)
}

// remoteCache holds other hosts' history, decrypted, in RAM ONLY: the plaintext
// is never written to disk (a hard requirement). What IS written to disk is the
// ciphertext it was decrypted from, in internal/rstore, which the server holds
// anyway and this machine could not read without its keys. That distinction is
// what makes the cache bounded. Cursors used to be RAM-only, so every daemon
// lifetime re-downloaded and re-decrypted every other machine's entire history
// from seq 0, and the daemon recycles on a 30-minute idle timeout. Now the
// cursor is persisted with the ciphertext, so a restart fetches only what is
// new, and `keep` caps how much of each host's tail is retained in either place.
// When sync is not configured the cache is disabled and reports state "off".
type remoteCache struct {
	mu sync.RWMutex
	// sy is nil until sync is configured. It is guarded by mu because the daemon
	// may attach or replace it at runtime when config.toml changes, while query
	// and status goroutines read it concurrently.
	sy      *syncer.Syncer
	records []rec.Record
	cmds    []string
	cursors map[string]uint64
	state   string
	lastMs  int64

	// rs is the on-disk ciphertext cache, or nil when it could not be opened,
	// in which case the daemon degrades to the old re-pull-everything behaviour
	// rather than refusing to run.
	rs *rstore.Store
	// keep caps retained records per remote host (0 = unlimited).
	keep int
	// dir is the state directory, needed to delete the ciphertext cache.
	dir string
	// st is the local store, whose meta bucket persists the revocation (see
	// metaRevoked). It lives in data.db rather than a file of its own so the
	// record is not a standalone thing to notice and delete.
	st *store.Store
	// revokedLatch stops THIS process retrying once the server has refused it as
	// revoked. It is deliberately not set by a revoked start: the persisted flag
	// records the last thing the server said, and a device re-enrolled since then
	// has to be able to find out, which costs exactly one refused request.
	revokedLatch bool
	// hydrated records whether the cached ciphertext has been decrypted into RAM
	// yet; that happens once per daemon lifetime, on the first sync that has keys.
	hydrated bool

	// tags is the shared server tag index; remote tag records are folded here so
	// tags applied on one machine resolve on this one too.
	tags *tagIndex
	// prompts is the shared prompt index; remote prompt records are folded here
	// so a command synced from another machine still shows the prompt behind it.
	prompts *promptIndex
}

// syncConf is the resolved subset of configuration that decides WHICH server the
// syncer talks to. It is comparable, so the daemon can distinguish a meaningful
// config change (re-attach) from an unrelated edit (leave the warm cache alone).
//
// There is no credential here: the device key IS the credential, so a machine
// that holds one and knows the server URL can sync. Enrollment tokens are used
// once by `yore setup` and never persisted.
type syncConf struct {
	url         string
	pin         string
	epoch       time.Duration
	syncPrompts bool
	keep        int
}

// configured reports whether there is enough configuration to sync at all.
func (sc syncConf) configured() bool { return sc.url != "" }

// sameServerAs reports whether sc points at the same server as prev: the test
// that decides whether the warm remote cache survives a configuration change.
// Only the URL identifies the server; every other field describes how to reach
// it (pin) or what to do once there (epoch, prompts, retention). A field added
// later that changes WHICH archive this machine syncs with belongs here too.
func (sc syncConf) sameServerAs(prev syncConf) bool { return sc.url == prev.url }

// loadSyncConf resolves the sync-relevant configuration from config.toml.
func loadSyncConf(dir string) syncConf {
	cfg, _ := config.Load(dir)
	return syncConf{
		url:         cfg.ServerURL,
		pin:         cfg.ServerPin,
		epoch:       cfg.KeyEpochD(),
		syncPrompts: cfg.SyncPrompts,
		keep:        cfg.RemoteKeepN(),
	}
}

// newSyncer builds a syncer for sc, or nil when sync is not configured or this
// machine has no usable device key: a machine can run purely local.
func newSyncer(dir string, st *store.Store, sc syncConf) *syncer.Syncer {
	if !sc.configured() {
		return nil
	}
	key, err := secret.Open(dir).LoadDeviceKey()
	if err != nil {
		return nil
	}
	sy := syncer.New(st, syncer.NewHTTPClient(sc.url, sc.pin), key, sc.epoch)
	sy.SetSyncPrompts(sc.syncPrompts)
	return sy
}

// newRemote builds the remote cache from persisted config. Missing server, token,
// or device key yields a disabled cache (state "off") rather than an error; the
// daemon re-checks the configuration as it runs, so sync configured later comes
// alive without a restart (see syncLoop). The on-disk ciphertext cache is opened
// best-effort: it is derived data, so a cache that cannot be opened (locked,
// corrupt) costs a full re-pull and must never stop the daemon starting. A device
// revoked in an earlier run starts here. onRevoked already deleted the cache when
// the server said so; this is the backstop for when it could not: the process
// died between recording and deleting, or the file was restored from a backup. It
// runs BEFORE the cache is opened, so the ciphertext leaves the disk even on a
// machine that comes back up offline and can never be told again. The state
// starts as revoked because that is the last thing the server said, but the cycle
// is NOT latched: a device enrolled again since then learns so on its first
// attempt, and a still-revoked one is simply refused again.
func newRemote(dir string, st *store.Store, sc syncConf, tags *tagIndex, prompts *promptIndex) *remoteCache {
	rc := &remoteCache{
		state:   proto.RemoteOff,
		cursors: map[string]uint64{},
		tags:    tags,
		prompts: prompts,
		keep:    sc.keep,
		dir:     dir,
		st:      st,
	}
	if sy := newSyncer(dir, st, sc); sy != nil {
		rc.sy = sy
		rc.state = proto.RemoteUnavailable // until the first successful sync
		if wasRevoked(st) {
			rc.state = proto.RemoteRevoked
			_ = rstore.Remove(dir)
		}
		if rs, err := rstore.Open(dir); err == nil {
			rc.rs = rs
		}
	}
	return rc
}

// metaRevoked is the local store's meta key recording that the sync server
// refused this device as revoked. It has to outlive the process that learned
// it: the response arrives while the daemon runs, but the cache is dropped at
// the next start too, which may be days later and offline with no server to
// ask. It lives in data.db's meta bucket, not a file of its own: the daemon
// holds that database under its write lock, so the record is not a loose file
// sitting in the state directory inviting deletion.
const metaRevoked = "revoked_by_server"

// wasRevoked reports whether a previous run recorded a revocation.
func wasRevoked(st *store.Store) bool {
	if st == nil {
		return false
	}
	v, err := st.Meta(metaRevoked)
	return err == nil && v != ""
}

// setRevokedMeta records or clears the revocation. Best-effort in both
// directions: a store that will not take the write must not turn a revocation
// into a crash, and the live cycle has already stopped syncing either way.
func setRevokedMeta(st *store.Store, revoked bool) {
	if st == nil {
		return
	}
	v := ""
	if revoked {
		v = "1"
	}
	_ = st.SetMeta(metaRevoked, v)
}

// onRevoked reacts to the server refusing this device as revoked. Retrying can
// never succeed, so the cycle stops for good and the group's ciphertext leaves
// this disk NOW: the cache is detached before the file is deleted, so nothing
// can write it back, and the cursors go with it (they describe streams this
// device may no longer read). The decrypted history already in RAM is left alone
// deliberately: it is what the user is looking at, and yanking it mid-session
// buys nothing that ending the session does not. It is gone at the next start,
// which finds no cache to hydrate from and no key to open one with. The meta
// record is written first and is the durable half: if this process dies between
// the two, the next start still knows to finish the job.
func (rc *remoteCache) onRevoked() {
	setRevokedMeta(rc.st, true)

	rc.mu.Lock()
	rs := rc.rs
	rc.rs = nil // detach before deleting: no later cycle may write it back
	rc.cursors = map[string]uint64{}
	rc.state = proto.RemoteRevoked
	rc.revokedLatch = true
	rc.mu.Unlock()

	if rs != nil {
		_ = rs.Close()
	}
	_ = rstore.Remove(rc.dir)
}

// onAccepted clears a recorded revocation once the server has served this device
// again: the only evidence that can retire it, and what lets a re-enrolled
// machine come back without anyone deleting anything by hand. Caller holds no
// lock; the store write is outside it.
func (rc *remoteCache) onAccepted() {
	if !wasRevoked(rc.st) {
		return
	}
	setRevokedMeta(rc.st, false)
}

// revoked reports whether this device is currently believed revoked.
func (rc *remoteCache) revoked() bool {
	if rc == nil {
		return false
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.state == proto.RemoteRevoked
}

// latched reports whether the server has refused this process as revoked, in
// which case no further cycle may run.
func (rc *remoteCache) latched() bool {
	if rc == nil {
		return false
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.revokedLatch
}

// close releases the on-disk ciphertext cache.
func (rc *remoteCache) close() {
	if rc == nil {
		return
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.rs != nil {
		_ = rc.rs.Close()
		rc.rs = nil
	}
}

// syncer returns the attached syncer, or nil when sync is not configured.
func (rc *remoteCache) syncer() *syncer.Syncer {
	if rc == nil {
		return nil
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.sy
}

func (rc *remoteCache) enabled() bool { return rc.syncer() != nil }

// online reports whether the server is currently believed reachable: the last
// sync attempt succeeded, or one is in flight. It gates the eager
// push-on-record nudge, while the server is unreachable, new local records
// simply stay spooled in the local store (already durable) and go out in a
// batch once the next periodic sync reconnects, instead of firing a push that
// would only fail against a dead server.
func (rc *remoteCache) online() bool {
	if rc == nil {
		return false
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	return rc.state == proto.RemoteOK || rc.state == proto.RemoteSyncing
}

// attach installs (or, with nil, clears) the syncer after the sync-relevant
// configuration changed, and applies the new retention bound.
//
// sameServer says whether the new configuration still points at the server the
// cache was filled from, and it decides the cache's fate:
//
//   - false: a different server (or none). Cached history and pull cursors are
//     dropped, in RAM and on disk both: they belong to the previous server, whose
//     key hierarchy has nothing to do with the new one's, and must never be mixed
//     with it. A revocation goes with them, marker and all: it was this device's
//     standing in the OLD group, and keeping it would leave a device that has
//     legitimately moved servers deleting its cache on every start.
//   - true: the same server, reached differently. A certificate pin added or
//     cleared, a rotation cadence, a retention bound. None of that invalidates
//     one byte of the ciphertext already cached, so throwing it away would cost a
//     full re-pull of every machine's archive for a transport edit, exactly what
//     the cache exists to avoid. It is kept, cursors and all, and so is any
//     revocation, which the same group's server would only tell us again.
//
// The cache is still self-correcting either way: ciphertext that turns out not to
// open (a re-enrollment the config never mentioned) is dropped by hydrate.
func (rc *remoteCache) attach(sy *syncer.Syncer, keep int, sameServer bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.sy = sy
	rc.keep = keep
	if !sameServer {
		rc.records = nil
		rc.cmds = nil
		rc.cursors = map[string]uint64{}
		rc.lastMs = 0
		rc.hydrated = false
		if rc.rs != nil {
			_ = rc.rs.Reset()
		}
		rc.revokedLatch = false
		setRevokedMeta(rc.st, false)
	}
	if sy == nil {
		rc.state = proto.RemoteOff
		return
	}
	// A revocation this run already acted on outlives a mere transport edit: the
	// ciphertext is off the disk and must not come back, and no cycle may run.
	if rc.revokedLatch {
		rc.state = proto.RemoteRevoked
		return
	}
	rc.state = proto.RemoteUnavailable
	// A revoked start left the cache deleted and detached; the new configuration
	// gets a working one back rather than re-pulling everything on every start.
	if rc.rs == nil {
		if rs, err := rstore.Open(rc.dir); err == nil {
			rc.rs = rs
		}
	}
}

// info reports the remote state for status/TUI display.
func (rc *remoteCache) info() proto.RemoteInfo {
	if rc == nil {
		return proto.RemoteInfo{State: proto.RemoteOff}
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	hosts := make(map[string]struct{})
	for i := range rc.records {
		hosts[rc.records[i].HostID] = struct{}{}
	}
	return proto.RemoteInfo{State: rc.state, LastSyncMs: rc.lastMs, Hosts: len(hosts)}
}

// syncOnce pushes local records and pulls remote ones into the cache. A
// decryption failure during pull is fatal for the cycle (never silently
// skipped) and leaves the cache as it was.
//
// The order matters: warm from the on-disk ciphertext first, so that a machine
// coming back after days offline decrypts what it already has before asking the
// server for anything, and then asks only for the delta.
func (rc *remoteCache) syncOnce(ctx context.Context, nowMs int64) error {
	sy := rc.syncer()
	if sy == nil {
		return errSyncOff
	}
	if rc.latched() {
		return errRevoked
	}
	// fail routes a cycle error: being revoked is terminal and gets its own
	// state, anything else is a transient the next cycle can retry.
	fail := func(err error) error {
		if syncer.ErrRevoked(err) {
			rc.onRevoked()
			return errRevoked
		}
		rc.setState(proto.RemoteUnavailable)
		return err
	}

	rc.setState(proto.RemoteSyncing)
	if _, err := sy.Push(ctx); err != nil {
		return fail(err)
	}
	if err := rc.hydrate(ctx, sy); err != nil {
		return fail(err)
	}

	rc.mu.RLock()
	cursors := cloneCursors(rc.cursors)
	rc.mu.RUnlock()

	byHost, next, err := sy.PullCiphertext(ctx, cursors)
	if err != nil {
		return fail(err)
	}

	var fresh []rec.Record
	for hostID, prs := range byHost {
		// Cache the ciphertext before decrypting it: if this process dies mid-cycle
		// the next one resumes from here instead of re-downloading the stream.
		rc.cacheCiphertext(hostID, prs, next[hostID])
		opened, oerr := sy.OpenRecords(ctx, hostID, prs)
		if oerr != nil {
			return fail(oerr)
		}
		fresh = append(fresh, opened...)
	}

	rc.mu.Lock()
	rc.foldRemote(fresh)
	rc.pruneLocked()
	rc.cursors = next
	rc.state = proto.RemoteOK
	rc.lastMs = nowMs
	rc.mu.Unlock()
	// The server served us, so any revocation we had recorded is history: this is
	// how a re-enrolled machine retires it without anyone editing state by hand.
	rc.onAccepted()
	return nil
}

// cacheCiphertext persists a host's freshly pulled sealed records and advances
// its stored cursor. Best-effort: the cache is derived, so a write failure
// costs a re-pull next time and nothing more.
func (rc *remoteCache) cacheCiphertext(hostID string, prs []wire.PullRecord, cursor uint64) {
	rc.mu.RLock()
	rs, keep := rc.rs, rc.keep
	rc.mu.RUnlock()
	if rs == nil {
		return
	}
	_, _ = rs.Append(hostID, prs, cursor, keep)
}

// hydrate decrypts the on-disk ciphertext cache into RAM, once per daemon
// lifetime. It runs on the first sync rather than at startup because unwrapping
// the keys needs the server.
//
// A decryption failure HERE is not treated as tampering, unlike one during a
// live pull: the only way this file can hold records we cannot open is if it
// outlived the group it belongs to (a re-enrollment the config did not
// register). It is derived data, so the honest response is to throw it away and
// re-pull, not to wedge sync forever on a stale cache.
func (rc *remoteCache) hydrate(ctx context.Context, sy *syncer.Syncer) error {
	rc.mu.Lock()
	if rc.hydrated {
		rc.mu.Unlock()
		return nil
	}
	rs := rc.rs
	if rs == nil {
		rc.hydrated = true
		rc.mu.Unlock()
		return nil
	}
	rc.mu.Unlock()

	cursors, err := rs.Cursors()
	if err != nil {
		return err
	}
	hosts, err := rs.Hosts()
	if err != nil {
		return err
	}

	var recs []rec.Record
	for _, hostID := range hosts {
		prs, rerr := rs.Records(hostID)
		if rerr != nil {
			return rerr
		}
		opened, oerr := sy.OpenRecords(ctx, hostID, prs)
		if oerr != nil {
			if resetErr := rs.Reset(); resetErr != nil {
				return resetErr
			}
			rc.mu.Lock()
			rc.hydrated = true
			rc.cursors = map[string]uint64{}
			rc.mu.Unlock()
			return nil // fall through to a full re-pull this cycle
		}
		recs = append(recs, opened...)
	}

	rc.mu.Lock()
	rc.foldRemote(recs)
	rc.cursors = cursors
	rc.hydrated = true
	rc.mu.Unlock()
	return nil
}

// search returns remote records matching q, optionally restricted to one
// hostname (empty = every remote host).
func (rc *remoteCache) search(q, hostname string) []rec.Record {
	if !rc.enabled() {
		return nil
	}
	query := match.Parse(q)
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	var out []rec.Record
	for i := range rc.records {
		if hostname != "" && rc.records[i].Hostname != hostname {
			continue
		}
		if query.Match(rc.cmds[i]) {
			out = append(out, rc.records[i])
		}
	}
	return out
}

// hostCounts aggregates the cache's records into per-host counts (carrying each
// host's HostID), for the browse HOSTS sidebar and Stats. Unlike search() it
// does NOT require enabled(), so a bare &remoteCache{records: …} answers without
// a syncer; the cache is already live-only (foldRemote drops tombstones), so it
// follows search()'s convention and does not re-filter Deleted() here. Returns
// nil when the cache is nil or empty.
func (rc *remoteCache) hostCounts() []proto.HostCount {
	if rc == nil {
		return nil
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	if len(rc.records) == 0 {
		return nil
	}
	type agg struct {
		hostID string
		count  int
	}
	counts := make(map[string]*agg)
	order := make([]string, 0, 4)
	for i := range rc.records {
		h := rc.records[i].Hostname
		a, ok := counts[h]
		if !ok {
			a = &agg{hostID: rc.records[i].HostID}
			counts[h] = a
			order = append(order, h)
		}
		a.count++
	}
	out := make([]proto.HostCount, 0, len(order))
	for _, h := range order {
		out = append(out, proto.HostCount{Hostname: h, HostID: counts[h].hostID, Count: counts[h].count})
	}
	return out
}

func (rc *remoteCache) setState(state string) {
	rc.mu.Lock()
	rc.state = state
	rc.mu.Unlock()
}

// foldRemote merges pulled records into the cache, keeping it live-only:
// tombstones remove their targets and are not themselves stored, and tag and
// prompt records go to their shared indexes rather than the command corpus.
// Caller holds the write lock.
func (rc *remoteCache) foldRemote(recs []rec.Record) {
	var deleted map[string]struct{}
	for i := range recs {
		if recs[i].Type == rec.TypeDelete && recs[i].TargetID != "" {
			if deleted == nil {
				deleted = make(map[string]struct{})
			}
			deleted[recs[i].TargetID] = struct{}{}
		}
		if recs[i].Type == rec.TypeTag && rc.tags != nil {
			rc.tags.apply(recs[i]) // fold remote tags into the shared index
		}
		if recs[i].Type == rec.TypePrompt && rc.prompts != nil {
			rc.prompts.apply(recs[i]) // likewise, so a synced command shows its prompt
		}
	}
	for i := range recs {
		r := recs[i]
		if !store.IsCommand(r) {
			continue
		}
		if _, gone := deleted[r.ID]; gone {
			continue
		}
		rc.records = append(rc.records, r)
		rc.cmds = append(rc.cmds, r.Cmd)
	}
	if len(deleted) > 0 {
		rc.keepLocked(func(r rec.Record) bool {
			_, gone := deleted[r.ID]
			return !gone
		})
		if rc.prompts != nil {
			rc.prompts.drop(deleted)
		}
	}
}

// pruneLocked evicts the oldest records of any host holding more than keep, so
// the decrypted cache stays bounded however long the daemon runs and however
// much history the group accumulates. Records arrive in ascending seq per host,
// so "oldest" is simply the leading ones. Caller holds the write lock.
func (rc *remoteCache) pruneLocked() {
	if rc.keep <= 0 {
		return
	}
	counts := make(map[string]int)
	for i := range rc.records {
		counts[rc.records[i].HostID]++
	}
	excess := make(map[string]int)
	for host, n := range counts {
		if n > rc.keep {
			excess[host] = n - rc.keep
		}
	}
	if len(excess) == 0 {
		return
	}
	rc.keepLocked(func(r rec.Record) bool {
		if excess[r.HostID] > 0 {
			excess[r.HostID]--
			return false
		}
		return true
	})
}

// keepLocked rebuilds the cache retaining the records keep reports true for,
// visiting them oldest-first. It allocates fresh slices rather than filtering
// in place so that a reader holding the previous slice header keeps seeing a
// coherent view. Caller holds the write lock.
func (rc *remoteCache) keepLocked(keep func(rec.Record) bool) {
	recs := make([]rec.Record, 0, len(rc.records))
	cmds := make([]string, 0, len(rc.records))
	for i := range rc.records {
		if !keep(rc.records[i]) {
			continue
		}
		recs = append(recs, rc.records[i])
		cmds = append(cmds, rc.records[i].Cmd)
	}
	rc.records, rc.cmds = recs, cmds
}

func cloneCursors(m map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// syncLoop drives periodic and on-demand sync. It runs even when sync is not
// configured, because its tick is also what notices sync being configured later;
// otherwise a daemon started before `yore setup` would stay local-only for its
// whole life. A single goroutine, so syncOnce never overlaps itself.
func (s *server) syncLoop() {
	defer s.wg.Done()
	cfg, _ := config.Load(s.dir)        // Defaults() on error, never an empty Config
	pushDebounce := cfg.PushDebounceD() // 0 = experimental push-on-record disabled

	// Kick an initial sync shortly after startup so deep search is warm.
	first := time.NewTimer(2 * time.Second)
	tick := time.NewTicker(syncTick(s.remote.enabled(), cfg.SyncIntervalD()))
	defer first.Stop()
	defer tick.Stop()

	// reload re-reads config.toml and re-attaches the syncer when the sync-relevant
	// configuration changed, so `yore setup` (or a hand edit) takes effect live.
	// Only a changed server URL invalidates the warm cache; everything else in
	// syncConf describes how to reach the same server, not which one.
	reload := func() {
		sc := loadSyncConf(s.dir)
		if sc == s.syncConf {
			return
		}
		sameServer := sc.sameServerAs(s.syncConf)
		s.syncConf = sc
		cfg, _ := config.Load(s.dir)
		pushDebounce = cfg.PushDebounceD()
		s.remote.attach(newSyncer(s.dir, s.store, sc), sc.keep, sameServer)
		enabled := s.remote.enabled()
		tick.Reset(syncTick(enabled, cfg.SyncIntervalD()))
		s.logf("config changed: sync enabled=%v server=%q cache_kept=%v", enabled, sc.url, sameServer)
	}

	// One-shot debounce timer for experimental push-on-record; starts idle.
	pushTimer := time.NewTimer(time.Hour)
	if !pushTimer.Stop() {
		<-pushTimer.C
	}
	defer pushTimer.Stop()
	pushPending := false

	for {
		select {
		case <-s.done:
			return
		case <-first.C:
			reload()
			_ = s.doSync() // logged inside; the loop retries on the next tick
		case <-tick.C:
			reload()
			_ = s.doSync() // logged inside; the loop retries on the next tick
		case <-s.syncWake:
			reload()
			_ = s.doSync() // logged inside; the loop retries on the next tick
		case <-s.pushWake:
			// Coalesce a burst of new records into one push after pushDebounce.
			// Arm only when enabled and not already pending: the timer is idle
			// at that point, so Reset is race-free.
			if arm, pending := pushArm(pushDebounce, pushPending); arm {
				pushTimer.Reset(pushDebounce)
				pushPending = pending
			}
		case <-pushTimer.C:
			pushPending = false
			_ = s.doSync() // logged inside; the loop retries on the next tick
		}
	}
}

// pushArm decides whether a pushWake should (re)arm the push-on-record debounce
// timer: only when it is enabled (debounce > 0) and no push is already pending,
// so a burst coalesces into a single push. Pure, for deterministic tests.
func pushArm(debounce time.Duration, pending bool) (arm, newPending bool) {
	if debounce <= 0 || pending {
		return false, pending
	}
	return true, true
}

// syncTick is the period between sync-loop wakeups: the configured sync
// interval when sync is live, otherwise a short poll that exists only to notice
// sync being configured (a stat of config.toml, not a network call).
func syncTick(enabled bool, interval time.Duration) time.Duration {
	if enabled {
		return interval
	}
	return unconfiguredPoll
}

// doSync runs one sync cycle and RETURNS its outcome. It is serialized by syncMu
// so the periodic loop and an explicit OpSync never overlap (which would
// double-push or race the pull cursors). It is a no-op when sync is not
// configured. The periodic loop ignores the error (it logs and retries); an
// explicit `yore sync` reports it, which is the whole point of asking.
func (s *server) doSync() error {
	if !s.remote.enabled() {
		return nil
	}
	// Once refused, the periodic loop must not log it on every tick for the rest
	// of the daemon's life. Whoever asked still gets the error.
	if s.remote.latched() {
		return errRevoked
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.remote.syncOnce(ctx, time.Now().UnixMilli()); err != nil {
		s.logf("sync error: %v", err)
		return err
	}
	info := s.remote.info()
	s.logf("sync ok: remote hosts=%d", info.Hosts)
	return nil
}
