package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"

	"go.etcd.io/bbolt"

	"yore/internal/reqsign"
	"yore/internal/wire"
)

const (
	maxPushRecords = 1000
	defaultLimit   = 1000
	maxLimit       = 1000
)

// GET /v1/health (no auth)
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// GET /v1/hosts
func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	db, ok := mustDB(w, r)
	if !ok {
		return
	}
	var hosts []wire.HostInfo
	err := db.View(func(tx *bbolt.Tx) error {
		return tx.ForEach(func(name []byte, b *bbolt.Bucket) error {
			n := string(name)
			if len(n) <= len(recordsPrefix) || n[:len(recordsPrefix)] != recordsPrefix {
				return nil
			}
			hostID := n[len(recordsPrefix):]
			k, _ := b.Cursor().Last()
			hosts = append(hosts, wire.HostInfo{HostID: hostID, MaxSeq: beUint64(k)})
			return nil
		})
	})
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	sort.Slice(hosts, func(i, j int) bool { return hosts[i].HostID < hosts[j].HostID })
	writeJSON(w, http.StatusOK, wire.HostsResp{Hosts: hosts})
}

// POST /v1/records
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	db, ok := mustDB(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if !s.requireSignature(w, r, db, body, nil) {
		return
	}
	var req wire.PushReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}
	if req.HostID == "" {
		writeErr(w, http.StatusBadRequest, "host_id required")
		return
	}
	if len(req.Records) > maxPushRecords {
		writeErr(w, http.StatusBadRequest, "too many records (max 1000)")
		return
	}
	for i := 1; i < len(req.Records); i++ {
		if req.Records[i].Seq <= req.Records[i-1].Seq {
			writeErr(w, http.StatusBadRequest, "records must be in strictly ascending seq order")
			return
		}
	}

	now := time.Now().UnixMilli()
	var resp wire.PushResp
	err := db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(recordsPrefix + req.HostID))
		if err != nil {
			return err
		}
		stored := 0
		for _, pr := range req.Records {
			key := seqKey(pr.Seq)
			if b.Get(key) != nil {
				continue // first write wins; idempotent skip
			}
			val, err := json.Marshal(storedRecord{
				ID:        pr.ID,
				KeyID:     pr.KeyID,
				Blob:      pr.Blob,
				CreatedMs: now,
			})
			if err != nil {
				return err
			}
			if err := b.Put(key, val); err != nil {
				return err
			}
			stored++
		}
		lastKey, _ := b.Cursor().Last()
		resp = wire.PushResp{Stored: stored, MaxSeq: beUint64(lastKey)}
		return nil
	})
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// GET /v1/records?host_id=X&after=N&limit=M
func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	db, ok := mustDB(w, r)
	if !ok {
		return
	}
	hostID := r.URL.Query().Get("host_id")
	after, err := parseUint(r.URL.Query().Get("after"), 0)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid after")
		return
	}
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid limit")
		return
	}

	resp := wire.PullResp{Records: []wire.PullRecord{}}
	err = db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(recordsPrefix + hostID))
		if b == nil {
			return nil // unknown host is not an error
		}
		hasMore := false
		c := b.Cursor()
		for k, v := c.Seek(seqKey(after + 1)); k != nil; k, v = c.Next() {
			if len(resp.Records) == limit {
				hasMore = true
				break
			}
			var sr storedRecord
			if err := json.Unmarshal(v, &sr); err != nil {
				return err
			}
			resp.Records = append(resp.Records, wire.PullRecord{
				Seq:       beUint64(k),
				ID:        sr.ID,
				KeyID:     sr.KeyID,
				Blob:      sr.Blob,
				CreatedMs: sr.CreatedMs,
			})
		}
		if hasMore {
			last := resp.Records[len(resp.Records)-1].Seq
			resp.NextAfter = &last
		}
		return nil
	})
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /v1/devices
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	db, ok := mustDB(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	var req wire.RegisterReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "id required")
		return
	}
	if len(req.PubKey) != 32 {
		writeErr(w, http.StatusBadRequest, "pub_key must be 32 bytes")
		return
	}
	if len(req.SignKey) != 32 {
		writeErr(w, http.StatusBadRequest, "sign_key must be 32 bytes")
		return
	}
	// Registration is self-signed: the device proves it holds the private key
	// for the sign_key it is registering, and must claim that same id.
	if reqsign.Device(r.Header) != req.ID {
		s.denySig(w, r, errDeviceMismatch)
		return
	}
	if !s.requireSignature(w, r, db, body, req.SignKey) {
		return
	}

	dev := wire.Device{
		ID:        req.ID,
		Name:      req.Name,
		PubKey:    req.PubKey,
		SignKey:   req.SignKey,
		Status:    wire.DevicePending,
		CreatedMs: time.Now().UnixMilli(),
	}
	err := db.Update(func(tx *bbolt.Tx) error {
		devB := tx.Bucket(bucketDevices)
		if devB.Get([]byte(req.ID)) != nil {
			return fail(http.StatusConflict, "device already registered")
		}
		val, err := json.Marshal(dev)
		if err != nil {
			return err
		}
		return devB.Put([]byte(req.ID), val)
	})
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, dev)
}

// GET /v1/devices
func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request) {
	db, ok := mustDB(w, r)
	if !ok {
		return
	}
	devices := []wire.Device{}
	err := db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bucketDevices).ForEach(func(_, v []byte) error {
			var d wire.Device
			if err := json.Unmarshal(v, &d); err != nil {
				return err
			}
			devices = append(devices, d)
			return nil
		})
	})
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	writeJSON(w, http.StatusOK, devices)
}

// bootstrapSelfKey returns the pending signer's own SignKey when this activate
// is the group-forming bootstrap — the device named in the path signs its own
// activation and no device is active yet (matching syncer.Bootstrap, where the
// first device self-activates while still pending). It returns nil in every
// other case, so ordinary activations fall through to requireSignature's
// active-signer requirement (any active device may approve any pending device).
func (s *Server) bootstrapSelfKey(db *bbolt.DB, r *http.Request, pathID string) []byte {
	signer := reqsign.Device(r.Header)
	if signer == "" || signer != pathID {
		return nil
	}
	var selfKey []byte
	_ = db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bucketDevices)
		active := false
		_ = b.ForEach(func(_, v []byte) error {
			var d wire.Device
			if json.Unmarshal(v, &d) == nil && d.Status == wire.DeviceActive {
				active = true
			}
			return nil
		})
		if active {
			return nil // not a bootstrap: an approver already exists
		}
		raw := b.Get([]byte(signer))
		if raw == nil {
			return nil
		}
		var d wire.Device
		if json.Unmarshal(raw, &d) == nil && d.Status == wire.DevicePending {
			selfKey = d.SignKey
		}
		return nil
	})
	return selfKey
}

// POST /v1/devices/{id}/activate
func (s *Server) handleActivate(w http.ResponseWriter, r *http.Request) {
	db, ok := mustDB(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if !s.requireSignature(w, r, db, body, s.bootstrapSelfKey(db, r, id)) {
		return
	}
	var req wire.ActivateReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}
	if req.Wrap.DeviceID != id {
		writeErr(w, http.StatusBadRequest, "wrap.device_id must equal path id")
		return
	}

	err := db.Update(func(tx *bbolt.Tx) error {
		devB := tx.Bucket(bucketDevices)
		raw := devB.Get([]byte(id))
		if raw == nil {
			return fail(http.StatusNotFound, "device not found")
		}
		var dev wire.Device
		if err := json.Unmarshal(raw, &dev); err != nil {
			return err
		}
		if dev.Status == wire.DeviceRevoked {
			return fail(http.StatusConflict, "device revoked")
		}

		hkB := tx.Bucket(bucketHKWraps)
		firstKey, _ := hkB.Cursor().First()
		if firstKey == nil {
			// Bootstrap: no wraps yet. Accept any version >= 1, set current.
			if req.Wrap.HKVersion < 1 {
				return fail(http.StatusBadRequest, "hk_version must be >= 1")
			}
			if err := setHKVersion(tx, req.Wrap.HKVersion); err != nil {
				return err
			}
		} else if req.Wrap.HKVersion != getHKVersion(tx) {
			return fail(http.StatusBadRequest, "hk_version must equal current")
		}

		wrapVal, err := json.Marshal(req.Wrap)
		if err != nil {
			return err
		}
		if err := hkB.Put([]byte(id), wrapVal); err != nil {
			return err
		}

		dev.Status = wire.DeviceActive
		devVal, err := json.Marshal(dev)
		if err != nil {
			return err
		}
		return devB.Put([]byte(id), devVal)
	})
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": wire.DeviceActive})
}

// POST /v1/devices/{id}/revoke
func (s *Server) handleRevoke(w http.ResponseWriter, r *http.Request) {
	db, ok := mustDB(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if !s.requireSignature(w, r, db, body, nil) {
		return
	}
	id := r.PathValue("id")
	err := db.Update(func(tx *bbolt.Tx) error {
		devB := tx.Bucket(bucketDevices)
		raw := devB.Get([]byte(id))
		if raw == nil {
			return fail(http.StatusNotFound, "device not found")
		}
		var dev wire.Device
		if err := json.Unmarshal(raw, &dev); err != nil {
			return err
		}
		dev.Status = wire.DeviceRevoked
		devVal, err := json.Marshal(dev)
		if err != nil {
			return err
		}
		if err := devB.Put([]byte(id), devVal); err != nil {
			return err
		}
		return tx.Bucket(bucketHKWraps).Delete([]byte(id))
	})
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": wire.DeviceRevoked})
}

// GET /v1/keys/hk?device_id=X
func (s *Server) handleGetHK(w http.ResponseWriter, r *http.Request) {
	db, ok := mustDB(w, r)
	if !ok {
		return
	}
	deviceID := r.URL.Query().Get("device_id")
	var wrap wire.HKWrap
	found := false
	err := db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bucketHKWraps).Get([]byte(deviceID))
		if v == nil {
			return nil
		}
		found = true
		return json.Unmarshal(v, &wrap)
	})
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "no hk wrap for device")
		return
	}
	writeJSON(w, http.StatusOK, wrap)
}

// GET /v1/keys/dek?cursor=K&limit=M
func (s *Server) handleListDEK(w http.ResponseWriter, r *http.Request) {
	db, ok := mustDB(w, r)
	if !ok {
		return
	}
	cursor := r.URL.Query().Get("cursor")
	limit, err := parseLimit(r.URL.Query().Get("limit"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid limit")
		return
	}

	resp := wire.DEKListResp{Wraps: []wire.DEKWrap{}}
	err = db.View(func(tx *bbolt.Tx) error {
		c := tx.Bucket(bucketDEKWraps).Cursor()
		var k, v []byte
		if cursor == "" {
			k, v = c.First()
		} else {
			k, v = c.Seek([]byte(cursor))
			if k != nil && string(k) == cursor {
				k, v = c.Next() // strictly after cursor
			}
		}
		hasMore := false
		for ; k != nil; k, v = c.Next() {
			if len(resp.Wraps) == limit {
				hasMore = true
				break
			}
			var dw wire.DEKWrap
			if err := json.Unmarshal(v, &dw); err != nil {
				return err
			}
			resp.Wraps = append(resp.Wraps, dw)
		}
		if hasMore {
			resp.NextCursor = resp.Wraps[len(resp.Wraps)-1].KeyID
		}
		return nil
	})
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// POST /v1/keys/dek
func (s *Server) handlePostDEK(w http.ResponseWriter, r *http.Request) {
	db, ok := mustDB(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if !s.requireSignature(w, r, db, body, nil) {
		return
	}
	var wraps []wire.DEKWrap
	if err := decodeJSON(r, &wraps); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}

	stored := 0
	err := db.Update(func(tx *bbolt.Tx) error {
		current := getHKVersion(tx)
		for _, dw := range wraps {
			if dw.KeyID == "" {
				return fail(http.StatusBadRequest, "key_id required")
			}
			if dw.DeviceID == "" {
				return fail(http.StatusBadRequest, "device_id required")
			}
			if dw.HKVersion != current {
				return fail(http.StatusBadRequest, "hk_version must equal current")
			}
		}
		b := tx.Bucket(bucketDEKWraps)
		for _, dw := range wraps {
			if b.Get([]byte(dw.KeyID)) != nil {
				continue // idempotent skip
			}
			val, err := json.Marshal(dw)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(dw.KeyID), val); err != nil {
				return err
			}
			stored++
		}
		return nil
	})
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"stored": stored})
}

// POST /v1/keys/rotate
func (s *Server) handleRotate(w http.ResponseWriter, r *http.Request) {
	db, ok := mustDB(w, r)
	if !ok {
		return
	}
	body, ok := readBody(w, r)
	if !ok {
		return
	}
	if !s.requireSignature(w, r, db, body, nil) {
		return
	}
	var req wire.RotateReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "malformed JSON: "+err.Error())
		return
	}

	err := db.Update(func(tx *bbolt.Tx) error {
		current := getHKVersion(tx)
		if req.HKVersion != current+1 {
			return fail(http.StatusBadRequest, "hk_version must be current+1")
		}
		if len(req.HKWraps) == 0 {
			return fail(http.StatusBadRequest, "hk_wraps must be non-empty")
		}

		devB := tx.Bucket(bucketDevices)
		for _, hw := range req.HKWraps {
			raw := devB.Get([]byte(hw.DeviceID))
			if raw == nil {
				return fail(http.StatusBadRequest, "hk wrap for unknown device "+hw.DeviceID)
			}
			var dev wire.Device
			if err := json.Unmarshal(raw, &dev); err != nil {
				return err
			}
			if dev.Status != wire.DeviceActive {
				return fail(http.StatusBadRequest, "hk wrap for non-active device "+hw.DeviceID)
			}
		}

		// DEKWraps must cover EXACTLY the full set of existing keyIDs.
		dekB := tx.Bucket(bucketDEKWraps)
		existing := map[string]bool{}
		var existingOrder []string
		c := dekB.Cursor()
		for k, _ := c.First(); k != nil; k, _ = c.Next() {
			id := string(k)
			existing[id] = true
			existingOrder = append(existingOrder, id)
		}
		provided := map[string]bool{}
		for _, dw := range req.DEKWraps {
			if provided[dw.KeyID] {
				return fail(http.StatusBadRequest, "duplicate dek wrap for key "+dw.KeyID)
			}
			provided[dw.KeyID] = true
			if !existing[dw.KeyID] {
				return fail(http.StatusBadRequest, "unexpected dek wrap for key "+dw.KeyID)
			}
		}
		for _, id := range existingOrder {
			if !provided[id] {
				return fail(http.StatusBadRequest, "missing dek wrap for key "+id)
			}
		}

		// Apply. Replace all hk_wraps (dropping absent devices), overwrite deks.
		hkB := tx.Bucket(bucketHKWraps)
		var oldKeys [][]byte
		hc := hkB.Cursor()
		for k, _ := hc.First(); k != nil; k, _ = hc.Next() {
			oldKeys = append(oldKeys, append([]byte(nil), k...))
		}
		for _, k := range oldKeys {
			if err := hkB.Delete(k); err != nil {
				return err
			}
		}
		for _, hw := range req.HKWraps {
			val, err := json.Marshal(hw)
			if err != nil {
				return err
			}
			if err := hkB.Put([]byte(hw.DeviceID), val); err != nil {
				return err
			}
		}
		for _, dw := range req.DEKWraps {
			val, err := json.Marshal(dw)
			if err != nil {
				return err
			}
			if err := dekB.Put([]byte(dw.KeyID), val); err != nil {
				return err
			}
		}
		return setHKVersion(tx, req.HKVersion)
	})
	if err != nil {
		writeAPIErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"hk_version": req.HKVersion})
}

// ---- query param parsing ----

func parseUint(s string, def uint64) (uint64, error) {
	if s == "" {
		return def, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

func parseLimit(s string) (int, error) {
	if s == "" {
		return defaultLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return defaultLimit, nil
	}
	if n > maxLimit {
		return maxLimit, nil
	}
	return n, nil
}
