// Package daemon is yore's long-lived background process. It is the ONLY
// process that owns the bbolt store; it keeps the live history in RAM and
// serves search queries over a unix socket speaking the internal/proto
// NDJSON protocol.
//
// Concurrency model: one goroutine per connection serves that connection's
// requests in order (so a connection's match.Filter is used single-threaded);
// a single ingest goroutine debounces spool drains; the main goroutine owns
// the idle timer and drives graceful shutdown. The RAM corpus is append-only
// and guarded by an RWMutex — see runQuery for why readers may snapshot the
// slice header under RLock and then scan without the lock.
package daemon

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"yore/internal/config"
	"yore/internal/match"
	"yore/internal/proto"
	"yore/internal/rec"
	"yore/internal/spool"
	"yore/internal/store"
)

const (
	defaultLimit      = 200                   // QueryReq.Limit == 0 falls back to this
	ingestDebounce    = 75 * time.Millisecond // straggler window after a wake
	defaultIdle       = 30 * time.Minute      // last-resort idle timeout
	snapshotInterval  = 5 * time.Minute       // periodic warm-snapshot write
	socketCheckPeriod = 30 * time.Second      // how often to confirm our socket still exists
	unconfiguredPoll  = 15 * time.Second      // sync-loop tick while sync is unconfigured
)

// Options configures a daemon run.
type Options struct {
	// IdleTimeout is how long the daemon waits with no requests before it
	// exits. Zero means "use config.Load(dir).DaemonIdleD()".
	IdleTimeout time.Duration
	// Version is reported in StatusResp.Version.
	Version string
}

// server holds all daemon state for one Run.
type server struct {
	dir         string
	opts        Options
	cfg         config.Config
	store       *store.Store
	ln          net.Listener
	sockInfo    os.FileInfo   // identity of the socket file we bound; see socketLive
	sockCheck   time.Duration // socketWatchLoop period; 0 means socketCheckPeriod
	logFile     io.Closer
	logger      *log.Logger
	idleTimeout time.Duration
	startTime   time.Time
	remote      *remoteCache
	// syncConf is the sync configuration the current syncer was built from.
	// Owned solely by the syncLoop goroutine, which compares it against disk to
	// detect a meaningful config change.
	syncConf syncConf

	// Corpus: live records in ascending seq, plus the parallel commands slice
	// that match.Filter scans. Append-only while running; guarded by mu.
	// lastSeq is the highest RAW seq folded in (incl. tombstones) and is only
	// ever touched by the ingest goroutine (and by setup before it starts).
	mu      sync.RWMutex
	corpus  []rec.Record
	cmds    []string
	lastSeq uint64

	// tags resolves user-tag associations (fed by both local ingest and remote
	// pull); read at query time to resolve each row's effective tags.
	tags *tagIndex

	// Live connections, so shutdown can unblock handlers parked in ReadMsg.
	conns   map[net.Conn]struct{}
	closing bool

	wake        chan struct{} // "spool has data" -> debounced ingest
	activity    chan struct{} // "a request arrived" -> reset idle timer
	shutdownReq chan struct{} // OpShutdown -> graceful stop
	syncWake    chan struct{} // "sync now" -> push/pull cycle
	pushWake    chan struct{} // new local records -> debounced push (experimental)
	sigCh       chan os.Signal
	done        chan struct{} // closed once, signals all workers to stop

	wg           sync.WaitGroup
	shutdownOnce sync.Once
	syncMu       sync.Mutex // serializes sync cycles (periodic loop vs explicit OpSync)
}

// Run opens the store and serves until idle, a signal, or OpShutdown. If
// another daemon already holds the store it returns nil silently — this is the
// spawn-race loser path (see cli/record.go spawnDaemon).
func Run(dir string, opts Options) error {
	st, err := store.Open(dir)
	if err != nil {
		if errors.Is(err, store.ErrLocked) {
			return nil // another daemon owns the store; lose quietly
		}
		return err
	}

	// Load config once: idle timeout, logging, and backups all read from it.
	cfg, cerr := config.Load(dir)
	if cerr != nil {
		cfg = config.Defaults() // a bad file never blocks startup
	}

	idle := opts.IdleTimeout
	if idle == 0 {
		idle = cfg.DaemonIdleD()
	}

	logFile, logger := openLog(dir, cfg)

	sc := loadSyncConf(dir)
	tags := newTagIndex()
	tags.setAutoTags(cfg.AutoTags)
	s := &server{
		dir:         dir,
		opts:        opts,
		cfg:         cfg,
		store:       st,
		logFile:     logFile,
		logger:      logger,
		idleTimeout: idle,
		startTime:   time.Now(),
		tags:        tags,
		remote:      newRemote(dir, st, sc, tags),
		syncConf:    sc,
		conns:       make(map[net.Conn]struct{}),
		wake:        make(chan struct{}, 1),
		activity:    make(chan struct{}, 1),
		shutdownReq: make(chan struct{}, 1),
		syncWake:    make(chan struct{}, 1),
		pushWake:    make(chan struct{}, 1),
		done:        make(chan struct{}),
	}

	// The store lock proves no live daemon owns the socket, so any file at the
	// socket path is stale — remove it before listening.
	_ = os.Remove(config.SocketPath(dir))
	ln, err := net.Listen("unix", config.SocketPath(dir))
	if err != nil {
		_ = st.Close()
		if logFile != nil {
			_ = logFile.Close()
		}
		// Linux caps unix socket paths at ~108 bytes; a deeply nested
		// YORE_DIR is the usual culprit and deserves a plain diagnosis.
		if len(config.SocketPath(dir)) > 100 {
			return fmt.Errorf("%w (socket path is %d bytes; unix sockets max out near 108 — use a shorter YORE_DIR)",
				err, len(config.SocketPath(dir)))
		}
		return err
	}
	_ = os.Chmod(config.SocketPath(dir), 0o600)
	s.ln = ln
	// Remember which file we bound so socketLive can tell it from a replacement.
	s.sockInfo, _ = os.Stat(config.SocketPath(dir))

	bail := func(e error) error {
		_ = ln.Close()
		_ = os.Remove(config.SocketPath(dir))
		_ = st.Close()
		if logFile != nil {
			_ = logFile.Close()
		}
		return e
	}

	// Drain the spool once, then load the corpus (warm snapshot if present).
	if n, ierr := st.IngestSpool(); ierr != nil {
		s.logf("startup ingest error: %v", ierr)
	} else if n > 0 {
		s.logf("startup ingested %d records", n)
	}
	if err := s.loadCorpus(); err != nil {
		return bail(err)
	}
	// Seed the tag index from the store: tag records are not part of the command
	// corpus (nor the warm snapshot), so they are folded from their own scan.
	if trecs, terr := st.TagRecords(); terr != nil {
		s.logf("tag seed error: %v", terr)
	} else {
		for i := range trecs {
			s.tags.apply(trecs[i])
		}
		if len(trecs) > 0 {
			s.logf("seeded tag index from %d tag records", len(trecs))
		}
	}
	s.logf("started pid=%d corpus=%d idle=%s", os.Getpid(), len(s.corpus), idle)

	s.sigCh = make(chan os.Signal, 1)
	signal.Notify(s.sigCh, syscall.SIGINT, syscall.SIGTERM)

	// The sync loop runs unconditionally: its tick also notices sync being
	// configured after the daemon started, so a daemon that predates `yore setup`
	// picks it up instead of staying local-only until it exits.
	s.wg.Add(5)
	go s.acceptLoop()
	go s.ingestLoop()
	go s.snapshotLoop()
	go s.socketWatchLoop()
	go s.syncLoop()
	s.logf("sync enabled=%v", s.remote.enabled())
	if s.cfg.BackupIntervalD() > 0 {
		s.wg.Add(1)
		go s.backupLoop()
		s.logf("backups enabled interval=%s keep=%d", s.cfg.BackupIntervalD(), s.cfg.BackupKeepN())
	}

	s.serve()
	return nil
}

// serve owns the idle timer and blocks until shutdown is triggered.
func (s *server) serve() {
	idle := time.NewTimer(s.idleTimeout)
	defer idle.Stop()
	for {
		select {
		case <-s.activity:
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(s.idleTimeout)
		case <-idle.C:
			s.shutdown()
			return
		case <-s.sigCh:
			s.shutdown()
			return
		case <-s.shutdownReq:
			s.shutdown()
			return
		}
	}
}

// shutdown gracefully stops the daemon: stop accepting, unblock handlers,
// wait for workers, remove the socket, close the store. Idempotent.
func (s *server) shutdown() {
	s.shutdownOnce.Do(func() {
		signal.Stop(s.sigCh)
		close(s.done)
		_ = s.ln.Close()

		s.mu.Lock()
		s.closing = true
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()

		s.wg.Wait()

		// Persist a final warm snapshot so the next start is instant. Written
		// after workers stop, so the corpus is quiescent.
		s.snapshotNow()

		// Only unlink the socket if it is still the file we bound: if another
		// daemon has taken the path over, removing it would strand that daemon
		// on an unreachable listener (the very wedge socketWatchLoop exists to
		// prevent).
		if s.socketLive() {
			_ = os.Remove(config.SocketPath(s.dir))
		}
		_ = s.store.Close()
		s.logf("stopped pid=%d", os.Getpid())
		if s.logFile != nil {
			_ = s.logFile.Close()
		}
	})
}

// acceptLoop accepts connections until the listener is closed.
func (s *server) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return // listener closed (shutdown) or a terminal accept error
		}
		s.mu.Lock()
		if s.closing {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.handle(conn)
	}
}

// handle serves one connection's requests in order.
func (s *server) handle(conn net.Conn) {
	defer s.wg.Done()
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()

	r := bufio.NewReader(conn)
	f := match.NewFilter() // per-connection: exploits monotonically-growing queries
	for {
		var req proto.Request
		if err := proto.ReadMsg(r, &req); err != nil {
			return // client hung up or sent garbage
		}
		s.signalActivity()
		resp, stop := s.dispatch(&req, f)
		if err := proto.WriteMsg(conn, resp); err != nil {
			return
		}
		if stop {
			s.triggerShutdown()
			return
		}
	}
}

// dispatch handles one request and reports whether it should trigger shutdown.
func (s *server) dispatch(req *proto.Request, f *match.Filter) (proto.Response, bool) {
	switch req.Op {
	case proto.OpPing:
		s.wakeIngest()
		return proto.Response{OK: true}, false

	case proto.OpRecord:
		// Keep one durability story: spool it (fsync) exactly as the CLI does,
		// then let the debounced ingest fold it in. A crash before ingest just
		// leaves it in the spool for next startup.
		if req.Record == nil {
			return proto.Response{Err: "record: nil record"}, false
		}
		if err := spool.Append(config.SpoolDir(s.dir), *req.Record); err != nil {
			return proto.Response{Err: "record: " + err.Error()}, false
		}
		s.wakeIngest()
		return proto.Response{OK: true}, false

	case proto.OpQuery:
		if req.Query == nil {
			return proto.Response{Err: "query: nil query"}, false
		}
		qr := s.runQuery(f, *req.Query)
		return proto.Response{OK: true, Query: &qr}, false

	case proto.OpHosts:
		hi := s.hosts()
		return proto.Response{OK: true, Hosts: &hi}, false

	case proto.OpDelete:
		if req.DeleteID == "" {
			return proto.Response{Err: "delete: empty id"}, false
		}
		if err := s.deleteRecord(req.DeleteID); err != nil {
			return proto.Response{Err: "delete: " + err.Error()}, false
		}
		return proto.Response{OK: true}, false

	case proto.OpDevices:
		di, err := s.listDevices()
		if err != nil {
			return proto.Response{Err: "devices: " + err.Error()}, false
		}
		return proto.Response{OK: true, Devices: &di}, false

	case proto.OpApprove:
		if err := s.deviceOp(req.DeviceID, true); err != nil {
			return proto.Response{Err: "approve: " + err.Error()}, false
		}
		return proto.Response{OK: true}, false

	case proto.OpRevoke:
		if err := s.deviceOp(req.DeviceID, false); err != nil {
			return proto.Response{Err: "revoke: " + err.Error()}, false
		}
		return proto.Response{OK: true}, false

	case proto.OpTicket:
		ti, err := s.mintTicket()
		if err != nil {
			return proto.Response{Err: "ticket: " + err.Error()}, false
		}
		return proto.Response{OK: true, Ticket: &ti}, false

	case proto.OpStatus:
		st := s.status()
		return proto.Response{OK: true, Status: &st}, false

	case proto.OpTags:
		ti := proto.TagsInfo{Tags: s.tags.list()}
		return proto.Response{OK: true, Tags: &ti}, false

	case proto.OpSync:
		// Explicit sync is synchronous: run a full cycle and respond after it
		// finishes, so `yore sync` reflects the real outcome.
		if s.remote.enabled() {
			s.doSync()
		}
		return proto.Response{OK: true}, false

	case proto.OpShutdown:
		return proto.Response{OK: true}, true

	default:
		return proto.Response{Err: "unknown op: " + req.Op}, false
	}
}

func (s *server) status() proto.StatusResp {
	s.mu.RLock()
	rows := len(s.corpus)
	s.mu.RUnlock()
	return proto.StatusResp{
		PID:       os.Getpid(),
		UptimeSec: int64(time.Since(s.startTime).Seconds()),
		LocalRows: rows,
		Remote:    s.remote.info(),
		Version:   s.opts.Version,
	}
}

// ingestLoop debounces "spool has data" wakes into a single fsync'd drain.
func (s *server) ingestLoop() {
	defer s.wg.Done()
	var timer *time.Timer
	var fire <-chan time.Time
	for {
		select {
		case <-s.done:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-s.wake:
			// Start a straggler window; a drain in flight or already scheduled
			// covers any further wakes, since IngestSpool drains everything.
			if timer == nil {
				timer = time.NewTimer(ingestDebounce)
				fire = timer.C
			}
		case <-fire:
			timer, fire = nil, nil
			s.doIngest()
		}
	}
}

func (s *server) doIngest() {
	n, err := s.store.IngestSpool()
	if err != nil {
		s.logf("ingest error: %v", err)
		return
	}
	if n == 0 {
		return
	}
	s.refreshCorpus()
	s.logf("ingested %d records", n)
	// Experimental push-on-record: nudge the sync loop that new local records
	// landed. Harmless when disabled — the loop only arms its debounce timer when
	// push_debounce is set (otherwise it just drains this). Only nudge while the
	// server is reachable: when it is offline the records stay spooled in the
	// local store and go out in a batch once the periodic sync reconnects, rather
	// than firing an eager push that would only fail.
	if s.remote.enabled() && s.remote.online() {
		nudge(s.pushWake)
	}
}

// refreshCorpus folds newly-ingested rows into the RAM corpus. Only the ingest
// goroutine calls this, so lastSeq needs no lock for reading here.
func (s *server) refreshCorpus() {
	rows, err := s.store.Since(s.lastSeq, 0)
	if err != nil {
		s.logf("since error: %v", err)
		return
	}
	if len(rows) == 0 {
		return
	}
	s.mu.Lock()
	s.foldRows(rows)
	s.mu.Unlock()
}

// foldRows merges raw-stream rows into the corpus, advancing lastSeq. It keeps
// the corpus live-only: tombstones and already-deleted rows are not added, and
// a tombstone whose target is present removes it. The caller must hold the
// write lock (or be in single-threaded startup). A deletion rebuilds the
// backing slices into a fresh array, which is safe for the lock-free readers
// in runQuery: an in-flight reader keeps scanning the array it snapshotted.
func (s *server) foldRows(rows []rec.Record) {
	var deleted map[string]struct{}
	for i := range rows {
		if rows[i].Seq > s.lastSeq {
			s.lastSeq = rows[i].Seq
		}
		if rows[i].Type == rec.TypeDelete && rows[i].TargetID != "" {
			if deleted == nil {
				deleted = make(map[string]struct{})
			}
			deleted[rows[i].TargetID] = struct{}{}
		}
		if rows[i].Type == rec.TypeTag {
			s.tags.apply(rows[i]) // fold user tags into the index, not the corpus
		}
	}
	for i := range rows {
		r := rows[i]
		if r.Type == rec.TypeDelete || r.Type == rec.TypeTag || r.DeletedMs != 0 {
			continue
		}
		if _, gone := deleted[r.ID]; gone {
			continue // recorded and tombstoned within this same batch
		}
		s.corpus = append(s.corpus, r)
		s.cmds = append(s.cmds, r.Cmd)
	}
	if len(deleted) > 0 {
		s.dropFromCorpus(deleted)
	}
}

// dropFromCorpus rebuilds corpus/cmds excluding the given record IDs. Caller
// holds the write lock.
func (s *server) dropFromCorpus(ids map[string]struct{}) {
	kept := make([]rec.Record, 0, len(s.corpus))
	cmds := make([]string, 0, len(s.corpus))
	for i := range s.corpus {
		if _, gone := ids[s.corpus[i].ID]; gone {
			continue
		}
		kept = append(kept, s.corpus[i])
		cmds = append(cmds, s.corpus[i].Cmd)
	}
	s.corpus, s.cmds = kept, cmds
}

// snapshotLoop persists the corpus periodically so restarts stay warm. The
// final shutdown snapshot is written by shutdown(); this only covers long
// uptimes and crash resilience.
func (s *server) snapshotLoop() {
	defer s.wg.Done()
	t := time.NewTicker(snapshotInterval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.snapshotNow()
		}
	}
}

// socketWatchLoop stops the daemon if the socket file it bound is unlinked or
// replaced. A unix listener survives its path being removed: the daemon would
// keep accepting on an unreachable socket AND keep the store lock, so every
// client gets ENOENT while every respawn loses the lock race and exits quietly
// — a wedge only a manual kill clears. Exiting instead releases the lock, and
// the next poke spawns a healthy daemon.
func (s *server) socketWatchLoop() {
	defer s.wg.Done()
	period := s.sockCheck
	if period <= 0 {
		period = socketCheckPeriod
	}
	t := time.NewTicker(period)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if !s.socketLive() {
				s.logf("socket %s gone or replaced; shutting down", config.SocketPath(s.dir))
				s.triggerShutdown()
				return
			}
		}
	}
}

// socketLive reports whether the socket path still refers to the very file this
// daemon bound. A missing path, or a different file at it (another daemon took
// over), both mean our listener is unreachable.
func (s *server) socketLive() bool {
	if s.sockInfo == nil {
		return true // identity unknown: never shut down on a guess
	}
	fi, err := os.Stat(config.SocketPath(s.dir))
	if err != nil {
		return false
	}
	return os.SameFile(s.sockInfo, fi)
}

// hosts aggregates live-record counts per host from BOTH the local RAM corpus
// and the RAM remote cache. Local host first (consumers assume index 0 is the
// local host); every other host — remote included — follows by descending
// count. Like runQuery's deep path, it warms a cold remote cache in the
// background so opening browse pulls other hosts' history.
func (s *server) hosts() proto.HostsInfo {
	type agg struct {
		hostID string
		count  int
	}
	s.mu.RLock()
	counts := make(map[string]*agg)
	order := make([]string, 0, 4)
	for i := range s.corpus {
		h := s.corpus[i].Hostname
		a, ok := counts[h]
		if !ok {
			a = &agg{hostID: s.corpus[i].HostID}
			counts[h] = a
			order = append(order, h)
		}
		a.count++
	}
	s.mu.RUnlock()

	localCounts := make([]proto.HostCount, 0, len(order))
	for _, h := range order {
		localCounts = append(localCounts, proto.HostCount{Hostname: h, HostID: counts[h].hostID, Count: counts[h].count})
	}

	// Warm a cold cache so the next open is richer; the call itself never blocks.
	if s.remote.enabled() {
		nudge(s.syncWake)
	}

	return proto.HostsInfo{
		Hosts:  mergeHostCounts(s.store.Hostname(), localCounts, s.remote.hostCounts()),
		Remote: s.remote.info(),
	}
}

// mergeHostCounts orders the browse HOSTS list: the local host leads (proto and
// the TUI assume index 0 is local), then every other host — local leftovers and
// remote alike — by descending count. A remote entry that duplicates the local
// hostname is dropped (we never pull our own stream, but don't double-count if
// it ever happens). Pure: no locks, no server state, so it is unit-testable.
func mergeHostCounts(local string, localCounts, remoteCounts []proto.HostCount) []proto.HostCount {
	out := make([]proto.HostCount, 0, len(localCounts)+len(remoteCounts))

	// Local host first, if it has any records.
	for _, hc := range localCounts {
		if hc.Hostname == local {
			out = append(out, hc)
			break
		}
	}

	rest := make([]proto.HostCount, 0, len(localCounts)+len(remoteCounts))
	for _, hc := range localCounts {
		if hc.Hostname != local {
			rest = append(rest, hc)
		}
	}
	for _, hc := range remoteCounts {
		if hc.Hostname != local { // defensive: never double-count the local host
			rest = append(rest, hc)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool { return rest[i].Count > rest[j].Count })

	return append(out, rest...)
}

// deleteRecord appends a tombstone (so the deletion syncs) and rebuilds the
// RAM corpus without the victim. Rare operation; the rebuild is O(corpus) and
// per-connection filters recover via their shrink-fallback rescan.
func (s *server) deleteRecord(id string) error {
	_, err := s.store.Append(rec.Record{
		Type:     rec.TypeDelete,
		TargetID: id,
		StartMs:  time.Now().UnixMilli(),
	})
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// The tombstone itself also consumed a seq; account for it so the next
	// refreshCorpus doesn't re-scan it into the corpus.
	if ls, lerr := s.store.LastSeq(); lerr == nil && ls > s.lastSeq {
		s.lastSeq = ls
	}
	s.dropFromCorpus(map[string]struct{}{id: {}})
	return nil
}

func (s *server) wakeIngest()      { nudge(s.wake) }
func (s *server) signalActivity()  { nudge(s.activity) }
func (s *server) triggerShutdown() { nudge(s.shutdownReq) }

// nudge does a non-blocking send: the channels are buffered(1) and every
// signal is idempotent, so a full buffer means the wake is already pending.
func nudge(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (s *server) logf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Printf(format, args...)
	}
}

// openLog prepares $YORE_DIR/daemon.log. Three modes, driven by config:
//   - LogSilent (the default): no file is created and a nil logger is returned,
//     so s.logf is a no-op (and the returned closer is nil).
//   - LogMaxBytes > 0: a size-capped rotating writer keeps the log bounded.
//   - otherwise: a plain 0600 append file (unbounded).
//
// A failure to open is non-fatal: the daemon runs without logging rather than
// refusing to start. The returned io.Closer is closed on shutdown (nil-safe).
func openLog(dir string, cfg config.Config) (io.Closer, *log.Logger) {
	if cfg.LogSilent {
		return nil, nil
	}
	path := filepath.Join(dir, "daemon.log")
	if maxBytes := cfg.LogMaxBytes(); maxBytes > 0 {
		w, err := newRotatingWriter(path, maxBytes, cfg.LogKeepN())
		if err != nil {
			return nil, nil
		}
		return w, log.New(w, "", log.LstdFlags)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil
	}
	return f, log.New(f, "", log.LstdFlags)
}
