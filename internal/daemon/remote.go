package daemon

import (
	"context"
	"errors"
	"sync"
	"time"

	"yore/internal/config"
	"yore/internal/cryptobox"
	"yore/internal/match"
	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/secret"
	"yore/internal/store"
	"yore/internal/syncer"
	"yore/internal/wire"
)

// errSyncOff is returned by device operations when sync isn't configured.
var errSyncOff = errors.New("sync not configured (run `yore setup`)")

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

// mintTicket issues a single-use enrollment ticket for adding another machine.
func (s *server) mintTicket() (proto.TicketInfo, error) {
	sy := s.remote.syncer()
	if sy == nil {
		return proto.TicketInfo{}, errSyncOff
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	t, err := sy.MintTicket(ctx)
	if err != nil {
		return proto.TicketInfo{}, err
	}
	return proto.TicketInfo{Ticket: t.Ticket, ExpiresMs: t.ExpiresMs}, nil
}

// remoteCache holds other hosts' history, decrypted, in RAM ONLY — it is never
// written to disk (a hard requirement). It is populated by pulling ciphertext
// from the sync server and decrypting via the syncer, and it is re-pulled from
// scratch each daemon lifetime (pull cursors are RAM-only). When sync is not
// configured the cache is disabled and reports state "off".
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
}

// syncConf is the resolved subset of configuration that decides WHICH server the
// syncer talks to. It is comparable, so the daemon can distinguish a meaningful
// config change (re-attach) from an unrelated edit (leave the warm cache alone).
//
// There is no credential here: the device key IS the credential, so a machine
// that holds one and knows the server URL can sync. Enrollment tickets are used
// once by `yore setup` and never persisted.
type syncConf struct {
	url   string
	pin   string
	epoch time.Duration
}

// configured reports whether there is enough configuration to sync at all.
func (sc syncConf) configured() bool { return sc.url != "" }

// loadSyncConf resolves the sync-relevant configuration from config.toml.
func loadSyncConf(dir string) syncConf {
	cfg, _ := config.Load(dir)
	return syncConf{url: cfg.ServerURL, pin: cfg.ServerPin, epoch: cfg.KeyEpochD()}
}

// newSyncer builds a syncer for sc, or nil when sync is not configured or this
// machine has no usable device key — a machine can run purely local.
func newSyncer(dir string, st *store.Store, sc syncConf) *syncer.Syncer {
	if !sc.configured() {
		return nil
	}
	key, err := secret.Open(dir).LoadDeviceKey()
	if err != nil {
		return nil
	}
	return syncer.New(st, syncer.NewHTTPClient(sc.url, sc.pin), key, sc.epoch)
}

// newRemote builds the remote cache from persisted config. Missing server,
// token, or device key yields a disabled cache (state "off") rather than an
// error; the daemon re-checks the configuration as it runs, so sync configured
// later comes alive without a restart (see syncLoop).
func newRemote(dir string, st *store.Store, sc syncConf) *remoteCache {
	rc := &remoteCache{state: proto.RemoteOff, cursors: map[string]uint64{}}
	if sy := newSyncer(dir, st, sc); sy != nil {
		rc.sy = sy
		rc.state = proto.RemoteUnavailable // until the first successful sync
	}
	return rc
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
// push-on-record nudge — while the server is unreachable, new local records
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
// configuration changed. Cached remote history and pull cursors are dropped:
// they belong to the previous server and must never be mixed with the new one's.
func (rc *remoteCache) attach(sy *syncer.Syncer) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.sy = sy
	rc.records = nil
	rc.cmds = nil
	rc.cursors = map[string]uint64{}
	rc.lastMs = 0
	if sy == nil {
		rc.state = proto.RemoteOff
	} else {
		rc.state = proto.RemoteUnavailable
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
func (rc *remoteCache) syncOnce(ctx context.Context, nowMs int64) error {
	sy := rc.syncer()
	if sy == nil {
		return errSyncOff
	}
	rc.setState(proto.RemoteSyncing)
	if _, err := sy.Push(ctx); err != nil {
		rc.setState(proto.RemoteUnavailable)
		return err
	}
	rc.mu.RLock()
	cursors := cloneCursors(rc.cursors)
	rc.mu.RUnlock()

	recs, next, err := sy.PullOthers(ctx, cursors)
	if err != nil {
		rc.setState(proto.RemoteUnavailable)
		return err
	}

	rc.mu.Lock()
	rc.foldRemote(recs)
	rc.cursors = next
	rc.state = proto.RemoteOK
	rc.lastMs = nowMs
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
// tombstones remove their targets and are not themselves stored. Caller holds
// the write lock.
func (rc *remoteCache) foldRemote(recs []rec.Record) {
	var deleted map[string]struct{}
	for i := range recs {
		if recs[i].Type == rec.TypeDelete && recs[i].TargetID != "" {
			if deleted == nil {
				deleted = make(map[string]struct{})
			}
			deleted[recs[i].TargetID] = struct{}{}
		}
	}
	for i := range recs {
		r := recs[i]
		if r.Type == rec.TypeDelete || r.DeletedMs != 0 {
			continue
		}
		if _, gone := deleted[r.ID]; gone {
			continue
		}
		rc.records = append(rc.records, r)
		rc.cmds = append(rc.cmds, r.Cmd)
	}
	if len(deleted) > 0 {
		keptR := rc.records[:0]
		keptC := rc.cmds[:0]
		for i := range rc.records {
			if _, gone := deleted[rc.records[i].ID]; gone {
				continue
			}
			keptR = append(keptR, rc.records[i])
			keptC = append(keptC, rc.records[i].Cmd)
		}
		rc.records, rc.cmds = keptR, keptC
	}
}

func cloneCursors(m map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// syncLoop drives periodic and on-demand sync. It runs even when sync is not
// configured, because its tick is also what notices sync being configured later
// — otherwise a daemon started before `yore setup` would stay local-only for its
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

	// reload re-reads config.toml and re-attaches the syncer when the server or
	// identity changed, so `yore setup` (or a hand edit) takes effect live.
	reload := func() {
		sc := loadSyncConf(s.dir)
		if sc == s.syncConf {
			return
		}
		s.syncConf = sc
		cfg, _ := config.Load(s.dir)
		pushDebounce = cfg.PushDebounceD()
		s.remote.attach(newSyncer(s.dir, s.store, sc))
		enabled := s.remote.enabled()
		tick.Reset(syncTick(enabled, cfg.SyncIntervalD()))
		s.logf("config changed: sync enabled=%v server=%q", enabled, sc.url)
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
			s.doSync()
		case <-tick.C:
			reload()
			s.doSync()
		case <-s.syncWake:
			reload()
			s.doSync()
		case <-s.pushWake:
			// Coalesce a burst of new records into one push after pushDebounce.
			// Arm only when enabled and not already pending — the timer is idle
			// at that point, so Reset is race-free.
			if arm, pending := pushArm(pushDebounce, pushPending); arm {
				pushTimer.Reset(pushDebounce)
				pushPending = pending
			}
		case <-pushTimer.C:
			pushPending = false
			s.doSync()
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

// doSync runs one sync cycle. It is serialized by syncMu so the periodic loop
// and an explicit OpSync never overlap (which would double-push or race the
// pull cursors). It is a no-op when sync is not configured.
func (s *server) doSync() {
	if !s.remote.enabled() {
		return
	}
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.remote.syncOnce(ctx, time.Now().UnixMilli()); err != nil {
		s.logf("sync error: %v", err)
		return
	}
	info := s.remote.info()
	s.logf("sync ok: remote hosts=%d", info.Hosts)
}
