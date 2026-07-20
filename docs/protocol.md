# yore sync protocol

The HTTP/JSON API between a `yore` client (the daemon's syncer) and the
`yore server`. The server stores **only ciphertext** — sealed record blobs,
wrapped keys, and device public keys — and can never read history. The Go types
are in `internal/wire`; the handlers are in `internal/server`.

## Conventions

- **Base URL**: whatever you deploy behind your reverse proxy, e.g.
  `https://yore.example.com`. All paths are under `/v1`.
- **Auth**: every endpoint except `GET /v1/health` requires
  `Authorization: Bearer <token>`, compared in constant time. The token is a
  single static secret (`$YORE_TOKEN` / `$YORE_TOKEN_FILE` on the server).
  Missing/incorrect → `401`.
- **Encoding**: request and response bodies are JSON. Binary fields (`blob`,
  `pub_key`) are Go `[]byte`, i.e. **base64** in JSON. Request bodies are capped
  at 10 MiB.
- **Errors**: any non-2xx response body is `{"error": "<message>"}`.
- **Model**: append-only per-host record streams with **client-assigned**
  sequence numbers; merge is a set-union by record ULID; deletions are appended
  tombstone records. Conflict-free, eventually consistent.

## Sync algorithm (how a client uses these)

**Push** (own stream): read the persisted watermark `last_uploaded_seq`, then
`POST /v1/records` in ascending batches of ≤1000, advancing the watermark after
each acked batch. Idempotent — re-pushing a stored `(host_id, seq)` is skipped.

**Pull** (other hosts): `GET /v1/hosts` for each stream's tip, then for every
host ≠ self page `GET /v1/records?host_id&after&limit` following `next_after`
until caught up; decrypt each blob into the RAM cache. Pull cursors are held in
daemon RAM only (remote history is never written to disk), so a fresh daemon
re-pulls from `after=0`.

**Enroll / key flow**: a new device `POST /v1/devices` (pending) → an existing
active device fetches its pubkey via `GET /v1/devices`, wraps the History Key
for it, and `POST /v1/devices/{id}/activate` → the new device can now
`GET /v1/keys/hk` and `GET /v1/keys/dek` to unwrap everything. **Revoke**:
`POST /v1/devices/{id}/revoke` then `POST /v1/keys/rotate` (new HK, all DEKs
re-wrapped) — records are never re-encrypted.

---

## Endpoints

### `GET /v1/health` — liveness (no auth)
→ `200 {"status":"ok"}`. Used by the container/Swarm healthcheck.

### `GET /v1/hosts` — stream tips
→ `200 {"hosts":[{"host_id":"01J…","max_seq":48211}, …]}`, sorted by host_id.
`max_seq` is 0 for a host that has never pushed.

### `POST /v1/records` — push a batch
Request `wire.PushReq`:
```json
{ "host_id": "01J…",
  "records": [ {"seq": 48212, "id": "01J…", "key_id": "01J…", "blob": "<base64>"}, … ] }
```
Rules: `host_id` required; ≤1000 records; **strictly ascending** `seq`.
Idempotent — an existing `(host_id, seq)` is skipped (first write wins).
→ `200 {"stored": 142, "max_seq": 48353}` (`stored` counts only newly-written rows).
Errors: `400` (missing host_id / >1000 / non-ascending / malformed).

### `GET /v1/records?host_id=X&after=N&limit=M` — pull a page
Returns records with `seq > after`, ascending. `limit` default/cap 1000.
→ `200`:
```json
{ "records": [ {"seq":48001,"id":"01J…","key_id":"01J…","blob":"<base64>","created_ms":1752…}, … ],
  "next_after": 49000 }
```
`next_after` is present only when more rows remain (omit → caught up). An unknown
`host_id` returns an empty list (not `404`). `created_ms` is the server receive
time (informational). Errors: `400` (invalid after/limit).

### `POST /v1/devices` — register (enroll)
Request `wire.RegisterReq` `{"id","name","pub_key":"<base64 32B>"}`.
→ `200 wire.Device` with `"status":"pending"`.
Errors: `400` (empty id / pub_key ≠ 32 bytes), `409` (id already registered).

### `GET /v1/devices` — list
→ `200 [wire.Device, …]` (all statuses), sorted by id. `Device` =
`{id, name, pub_key, status, created_ms}`; `status` ∈ `pending|active|revoked`.

### `POST /v1/devices/{id}/activate` — approve
Request `wire.ActivateReq` `{"wrap": <HKWrap>}` where `HKWrap` =
`{device_id, blob, hk_version}` — the History Key sealed to this device's pubkey.
Rules: `wrap.device_id` must equal `{id}`; device must exist and not be revoked;
if any HK wrap already exists, `hk_version` must equal the current version;
otherwise this is the **bootstrap** (first device) and any `hk_version ≥ 1` is
accepted and pinned. Stores the wrap and sets the device `active`.
→ `200 {"status":"active"}`.
Errors: `400` (device_id mismatch / bad version), `404` (unknown), `409` (revoked).

### `POST /v1/devices/{id}/revoke` — revoke
Sets the device `revoked` and deletes its HK wrap. → `200 {"status":"revoked"}`.
Errors: `404` (unknown). Follow with `POST /v1/keys/rotate`.

### `GET /v1/keys/hk?device_id=X` — this device's wrapped History Key
→ `200 wire.HKWrap`. `404` if none (pending / revoked / never wrapped).

### `GET /v1/keys/dek?cursor=K&limit=M` — list wrapped epoch data keys
Ordered by `key_id` (ULIDs = time-ordered), strictly **after** `cursor`
(`""`/omitted = from the start). `limit` default/cap 1000.
→ `200`:
```json
{ "wraps": [ {"key_id":"01J…","device_id":"01J…","epoch":1752…,"blob":"<base64>","hk_version":1}, … ],
  "next_cursor": "01J…" }
```
`next_cursor` (the last returned `key_id`) is present only when more remain.

### `POST /v1/keys/dek` — upload wrapped data keys
Request `[]wire.DEKWrap`. Each must have non-empty `key_id`/`device_id` and
`hk_version` == current. Idempotent by `key_id`.
→ `200 {"stored": N}`. Errors: `400` (missing fields / wrong version).

### `POST /v1/keys/rotate` — atomic re-key after a revoke
Request `wire.RotateReq` `{hk_version, hk_wraps:[HKWrap], dek_wraps:[DEKWrap]}`.
All-or-nothing in one transaction. Rules:
- `hk_version` must be **current + 1**;
- `hk_wraps` non-empty, and every `device_id` in it currently **active**;
- `dek_wraps` must cover **exactly** the existing set of `key_id`s (no missing,
  extra, or duplicate — the error names the first offender).
On success: all HK wraps replaced (devices absent from `hk_wraps` lose access),
all DEK wraps overwritten under the new HK, version bumped. Records untouched.
→ `200 {"hk_version": N}`. Errors: `400` (any rule violation).

---

## Server storage (bbolt, ciphertext only)

- `devices`: device_id → `Device`
- `hk_wraps`: device_id → `HKWrap`
- `dek_wraps`: key_id → `DEKWrap`
- `records:<host_id>` (one bucket per stream): seq (8-byte BE) → `{id, key_id, blob, created_ms}`
- `meta`: `hk_version`

`max_seq` per host is the last key of its stream bucket — no separate index to
keep consistent. Exactly one server replica (bbolt is single-owner); TLS
terminates at your reverse proxy.
