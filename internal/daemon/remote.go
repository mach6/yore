package daemon

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"yore/internal/config"
	"yore/internal/cryptobox"
	"yore/internal/match"
	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/store"
	"yore/internal/syncer"
	"yore/internal/wire"
)

// errSyncOff is returned by device operations when sync isn't configured.
var errSyncOff = errors.New("sync not configured (run `yore setup`)")

// listDevices returns the enrolled devices with per-device verification codes
// for pending ones (computed here so proto stays independent of wire/cryptobox).
func (s *server) listDevices() (proto.DevicesInfo, error) {
	if !s.remote.enabled() {
		return proto.DevicesInfo{}, errSyncOff
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	devs, err := s.remote.sy.Devices(ctx)
	if err != nil {
		return proto.DevicesInfo{}, err
	}
	self := s.remote.sy.DeviceID()
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
	if !s.remote.enabled() {
		return errSyncOff
	}
	if id == "" {
		return errors.New("empty device id")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if approve {
		return s.remote.sy.Approve(ctx, id)
	}
	return s.remote.sy.Revoke(ctx, id)
}

// remoteCache holds other hosts' history, decrypted, in RAM ONLY — it is never
// written to disk (a hard requirement). It is populated by pulling ciphertext
// from the sync server and decrypting via the syncer, and it is re-pulled from
// scratch each daemon lifetime (pull cursors are RAM-only). When sync is not
// configured the cache is disabled and reports state "off".
type remoteCache struct {
	sy *syncer.Syncer

	mu      sync.RWMutex
	records []rec.Record
	cmds    []string
	cursors map[string]uint64
	state   string
	lastMs  int64
}

// newRemote builds the remote cache from persisted config. Missing server,
// token, or device key yields a disabled cache (state "off") rather than an
// error — a machine can run purely local.
func newRemote(dir string, st *store.Store) *remoteCache {
	rc := &remoteCache{state: proto.RemoteOff, cursors: map[string]uint64{}}
	cfg, _ := config.Load(dir)
	if cfg.ServerURL == "" {
		return rc
	}
	token := cfg.Token
	if token == "" {
		token = os.Getenv("YORE_TOKEN")
	}
	if token == "" {
		return rc
	}
	key, err := cryptobox.LoadDeviceKey(config.KeyPath(dir))
	if err != nil {
		return rc
	}
	rc.sy = syncer.New(st, syncer.NewHTTPClient(cfg.ServerURL, token), key, cfg.KeyEpochD())
	rc.state = proto.RemoteUnavailable // until the first successful sync
	return rc
}

func (rc *remoteCache) enabled() bool { return rc != nil && rc.sy != nil }

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
	rc.setState(proto.RemoteSyncing)
	if _, err := rc.sy.Push(ctx); err != nil {
		rc.setState(proto.RemoteUnavailable)
		return err
	}
	rc.mu.RLock()
	cursors := cloneCursors(rc.cursors)
	rc.mu.RUnlock()

	recs, next, err := rc.sy.PullOthers(ctx, cursors)
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

// syncLoop drives periodic and on-demand sync. Started only when the remote is
// enabled. A single goroutine, so syncOnce never overlaps itself.
func (s *server) syncLoop() {
	defer s.wg.Done()
	interval := config.Config{}.SyncIntervalD()
	if cfg, err := config.Load(s.dir); err == nil {
		interval = cfg.SyncIntervalD()
	}
	// Kick an initial sync shortly after startup so deep search is warm.
	first := time.NewTimer(2 * time.Second)
	tick := time.NewTicker(interval)
	defer first.Stop()
	defer tick.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-first.C:
			s.doSync()
		case <-tick.C:
			s.doSync()
		case <-s.syncWake:
			s.doSync()
		}
	}
}

// doSync runs one sync cycle. It is serialized by syncMu so the periodic loop
// and an explicit OpSync never overlap (which would double-push or race the
// pull cursors).
func (s *server) doSync() {
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
