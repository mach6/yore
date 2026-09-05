# yore protocols

The exact formats. Three things live here:

1. **The sync HTTP API** between a client (the daemon's syncer) and
   `yore server`. Types in `internal/wire`, handlers in `internal/server`.
2. **The crypto scheme** — the exact bytes a client seals before anything
   crosses the wire (`internal/cryptobox`, `internal/reqsign`).
3. **The daemon's local unix-socket protocol** — how the CLI and TUIs drive the
   background daemon (`internal/proto`). Local only, unrelated to the HTTP API.

The server stores only ciphertext — sealed record blobs, wrapped keys, and
device public keys — and can never read history. Why any of it is shaped this way
is in [`architecture.md`](architecture.md).

---

# Part 1 — The sync HTTP API

## Conventions

**Base URL.** Whatever you deploy behind your reverse proxy, e.g.
`https://yore.example.com`. All paths are under `/v1`.

**Auth.** One layer, no bearer token. Every endpoint except `GET /v1/health`,
`GET /v1/ready`, and `GET /v1/recovery/salt` requires a per-device Ed25519
signature — reads included. The device record *is* the credential, so no
long-lived shared secret exists to capture, and the signer also selects the
tenant. Two request classes predate having a device record and carry their own
authorization: **enrollment** (`POST /v1/devices`) presents a single-use token in
`X-Yore-Token`, and **recovery** signs with the key derived from the recovery
passphrase. Any failure is a generic `401`, so it never reveals which check
failed.

**Certificate pinning (optional).** A client enrolled with `yore setup --pin`
pins the server's TLS SPKI (base64 SHA-256 of `RawSubjectPublicKeyInfo`) and
refuses any other certificate, at the cost of not syncing through an inspecting
proxy. The pin is checked *in addition to* normal chain validation, so a pinned
certificate must still be issued by a trusted CA. It is keyed on the public key,
not the certificate, so a renewal reusing the key keeps the pin — but an ACME
client that generates a fresh key per renewal invalidates it every time. A
mismatch fails closed and quietly: this host's own history is unaffected, so the
only symptom is other machines' history going stale. `yore doctor` reports it as
a pin mismatch, printing both digests, rather than as an unreachable server.

**Encoding.** JSON bodies. Binary fields (`blob`, `pub_key`, `sign_key`) are Go
`[]byte`, i.e. standard padded base64 in JSON. Request bodies are capped at
**10 MiB**.

**Errors.** Any non-2xx body is `{"error": "<message>"}` (`wire.ErrorResp`).

**Model.** Append-only per-host record streams with client-assigned `seq`; merge
is a set union by record ULID; deletions are appended tombstone records.
Conflict-free and eventually consistent.

## Request signing (`internal/reqsign`)

Signed requests carry four headers:

```
X-Yore-Device:    <device id>
X-Yore-Timestamp: <unix seconds>
X-Yore-Nonce:     <base64url of 16 random bytes, single-use>
X-Yore-Signature: <base64url Ed25519 signature>
```

The signature covers this canonical byte string, newline-separated:

```
<method>\n<RequestURI>\n<timestamp>\n<nonce>\n<hex(sha256(body))>
```

`<RequestURI>` is `net/http`'s `Request.URL.RequestURI()` — path plus raw query,
no scheme or host — which is identical on both ends of a normal reverse proxy.
The body hash is lowercase hex SHA-256. The signature is made with the device's
Ed25519 private key, which is never on the wire.

Server verification, in order:

1. Resolve the signer's Ed25519 public key. For `register` it is the `sign_key`
   in the body (self-signed) and `X-Yore-Device` must equal the registered `id`.
   For the bootstrap `activate` it is the pending signer's own `sign_key` — the
   first device self-activates while no device is active yet. Otherwise it is the
   `sign_key` of the device named in `X-Yore-Device`, which must be **active**.
2. Verify the signature over the canonical string.
3. Reject a timestamp outside ±5 minutes (`reqsign.Skew`).
4. Reject a repeated `(device, nonce)` from the in-memory replay cache. Entries
   expire after `Skew`, so the cache stays bounded by request rate over 5
   minutes.

A proxy that captures traffic can therefore neither forge a request nor replay a
captured one, and there is no token for it to steal in the first place.

**Standing is checked after the signature, not before.** A caller whose signature
does not verify — including one guessing at a device id — gets the same opaque
`401 unauthorized` whether that id is unknown, pending, or revoked, so membership
cannot be probed. A caller whose signature *does* verify is the holder of that
device's private key, and a revoked one is told exactly that:

```json
403 {"error": "device revoked", "code": "device_revoked"}
```

That is the only way a revoked machine learns it should stop syncing and delete
its cached ciphertext; without it, it sits on that cache indefinitely and its
sync failures are indistinguishable from the server being down. `ErrorResp.Code`
is set only where the client is expected to *act* on the reason rather than
report it.

## How a client uses these

**Push** (own stream): read the persisted `last_uploaded_seq` watermark, then
`POST /v1/records` in ascending batches, advancing the watermark only after each
acked batch. Idempotent — re-pushing a stored `(host_id, seq)` is skipped, so a
failed batch is safely retried.

**Pull** (other hosts): `GET /v1/hosts` for each stream's tip, then for every host
other than self, page `GET /v1/records?host_id&after&limit` following
`next_after` until caught up. Pull cursors are **persisted** alongside the
downloaded ciphertext in the client's `remote.db`, so a restarting daemon resumes
where it left off rather than re-pulling from `after=0`. That cache is derived
data — deleting it costs one full re-pull.

**Enroll and key flow**: an enrolled device mints a single-use token
(`POST /v1/tokens`); the newcomer presents it on `POST /v1/devices` and lands
*pending*. The registration response's `group_formed` tells the newcomer which
path it is on, since it cannot yet read anything. An existing active device
fetches the newcomer's public key via `GET /v1/devices`, wraps the History Key
for it, and calls `POST /v1/devices/{id}/activate`. The new device can then
`GET /v1/keys/hk` and page `GET /v1/keys/dek` to unwrap everything.

**Bootstrap**: the very first device has no one to mint it a token, so the
server's configured token is accepted — but **only while the tenant has no active
device**. Once one exists that token enrolls nothing, which is what makes it a
first credential rather than a standing one.

**Revoke**: `POST /v1/devices/{id}/revoke`, then `POST /v1/keys/rotate` (new HK,
all data keys re-wrapped). Records are never re-encrypted.

When a cycle runs is a client concern; see `architecture.md`.

## Endpoints

### `GET /v1/health` — liveness (no auth)

→ `200 {"status":"ok"}`. Backs the container and CLI healthcheck. Deliberately
shallow: it touches no database, because a restart is the only remedy its
consumer has, and the failures it must catch are the ones a restart fixes.

### `GET /v1/ready` — storage still accepts a write (no auth)

→ `200 {"status":"ok"}`, or `503 {"error":"storage unavailable"}`.

A bbolt read is served from the existing mmap and needs no write at all, so a
server whose volume has filled — or gone read-only, or started failing I/O —
answers every GET perfectly while every mutation fails with `500`. Liveness
cannot see that, and neither can a client: the archive stays readable and
silently stops recording. This endpoint commits a one-key transaction to every
tenant database and reports whether it worked. The verdict is cached for 10s so
an open endpoint cannot be turned into a write amplifier, and the cause goes to
the server log, never the response.

It is **not** wired to the container healthcheck, on purpose. A full volume is
not something a restart repairs — bucket creation happens on the way up, so a
restart turns a degraded-but-readable server into a crash loop. Point monitoring
(and `yore doctor`, which calls it) here; leave the restart trigger on
`/v1/health`.

### `GET /v1/hosts` — stream tips

→ `200 {"hosts":[{"host_id":"01J…","max_seq":48211}, …]}`, sorted by `host_id`.
`max_seq` is the last key of the host's stream bucket, 0 if it never pushed.

### `POST /v1/records` — push a batch *(signed)*

Request `wire.PushReq`:

```json
{ "host_id": "01J…",
  "records": [ {"seq": 48212, "id": "01J…", "key_id": "01J…", "blob": "<base64>"}, … ] }
```

Rules: `host_id` required, ≤1000 records, strictly ascending `seq`, body ≤10 MiB.
Idempotent — an existing `(host_id, seq)` is skipped, first write wins.

→ `200 {"stored": 142, "max_seq": 48353}` (`stored` counts only newly written
rows). Errors: `400` (missing host_id, >1000, non-ascending, malformed JSON),
`413` (body over the cap).

A record count alone does not bound a body: a thousand records carrying long
prompts or heredocs is megabytes. The client therefore batches on **both** ≤1000
records and ≤8 MiB of encoded body, and on a `413` (or the `400` a truncated body
decodes as) halves the batch and retries, down to a single record — at which
point the error is real and is surfaced. This matters because the push watermark
only advances over what the server acked, so a body that can never fit would
otherwise fail every retry identically and wedge sync permanently.

### `GET /v1/records?host_id=X&after=N&limit=M` — pull a page

Returns records with `seq > after`, ascending. `limit` defaults to and is capped
at 1000.

```json
{ "records": [ {"seq":48001,"id":"01J…","key_id":"01J…","blob":"<base64>","created_ms":1752…}, … ],
  "next_after": 49000 }
```

`next_after` is present only when more rows remain; omitted means caught up. An
unknown `host_id` returns an empty list, not `404`. `created_ms` is the server
receive time and is informational. Errors: `400` (invalid `after`/`limit`).

### `POST /v1/devices` — register / enroll *(self-signed + token)*

Request `wire.RegisterReq`
`{"id","name","pub_key":"<base64 32B X25519>","sign_key":"<base64 32B Ed25519>"}`
plus `X-Yore-Token: <token>`. Must carry a valid signature made with the private
key for `sign_key`, and `X-Yore-Device` must equal `id`. The token is redeemed in
the **same transaction as the write**, so it can never admit two devices.

→ `200 wire.RegisterResp` `{device, group_formed}`; `group_formed` false means
this device should form the group. Errors: `400` (empty id, keys not 32B, device
id mismatch), `401` (bad signature, or an unknown/redeemed/expired token), `409`
(id already registered).

### `POST /v1/tokens` — mint an enrollment token *(signed)*

→ `200 wire.TokenResp` `{token, expires_ms}`. Valid 30 minutes, single use. The
plaintext is returned exactly once; the server stores only
`sha256("yore/token/v1|" ‖ token)`, so a database read yields no usable token —
and a token can never be shown again after the moment it was minted.

The stored record is `{created_ms, expires_ms, claimed_ms, claimed_by,
revoked_ms}`. `claimed_by` is the id of the device that enrolled on it, stamped
in the same transaction as the registration. Retention is applied on each mint:
an unclaimed token is deleted 7 days after it expires; a claimed one is kept,
because "which token admitted this machine" should outlive the half hour the
token was good for.

### `GET /v1/tokens` — list enrollment tokens *(signed)*

→ `200 [wire.EnrollToken, …]`, newest first, where `EnrollToken` is `{id, state,
created_ms, expires_ms, claimed_ms, claimed_by, revoked_ms}`. `id` is the hex of
the stored hash — safe to publish, since enrolling requires presenting the
plaintext; the id is a handle, not a credential. `state` is one of
`open|claimed|expired|revoked`, computed server-side so every client agrees on
what "expired" means without consulting its own clock. Claimed and revoked are
terminal and outrank expiry.

An open token is a standing invitation into the group, which is why this is
readable at all — and why it is signed rather than bearer-only.

### `POST /v1/tokens/{id}/revoke` — cancel an unused token *(signed)*

→ `204`. Idempotent. `400` for a malformed id, `404` if unknown, `409` if already
claimed — revoking then would say something untrue about how that machine got in
and take nothing away from it. Unlike device revocation this rotates nothing: the
token admitted no one, so there is no key a holder could already have read with.

### `GET /v1/devices` — list *(signed)*

→ `200 [wire.Device, …]` (all statuses), sorted by id. `Device` is
`{id, name, pub_key, sign_key, status, created_ms}` with `status` one of
`pending|active|revoked`.

### `POST /v1/devices/{id}/activate` — approve *(signed)*

Request `wire.ActivateReq` `{"wrap": <HKWrap>}`, where `HKWrap` is
`{device_id, blob, hk_version}` — the History Key sealed to this device's public
key.

Rules: `wrap.device_id` must equal `{id}`; the device must exist and not be
revoked; if any HK wrap already exists, `hk_version` must equal the current
version; otherwise this is the bootstrap (first device, signed by the pending
device itself) and any `hk_version ≥ 1` is accepted and pinned. Stores the wrap
and sets the device active.

→ `200 {"status":"active"}`. Errors: `400` (device id mismatch, bad version),
`404` (unknown), `409` (revoked).

### `POST /v1/devices/{id}/revoke` — revoke *(signed)*

Sets the device revoked and deletes its HK wrap. → `200 {"status":"revoked"}`.
`404` if unknown. Follow with `POST /v1/keys/rotate`.

### `GET /v1/keys/hk?device_id=X` — this device's wrapped History Key *(signed)*

→ `200 wire.HKWrap`. `404` if none, meaning pending or never wrapped; the client
treats that as "not activated". A revoked caller never reaches the handler — the
signature check refuses it first with `403 device_revoked`.

### `GET /v1/keys/dek?cursor=K&limit=M` — list wrapped epoch data keys *(signed)*

Ordered by `key_id` (ULIDs sort by creation time), strictly after `cursor`
(empty or omitted starts from the beginning). `limit` defaults to and is capped
at 1000.

```json
{ "wraps": [ {"key_id":"01J…","device_id":"01J…","epoch":1752…,"blob":"<base64>","hk_version":1}, … ],
  "next_cursor": "01J…" }
```

`next_cursor` is the last returned `key_id`, present only when more remain.

### `POST /v1/keys/dek` — upload wrapped data keys *(signed)*

Request `[]wire.DEKWrap`. Each must have a non-empty `key_id` and `device_id`,
and `hk_version` equal to the current version. Idempotent by `key_id`.
→ `200 {"stored": N}`. Errors: `400` (missing fields, wrong version).

### `POST /v1/keys/rotate` — atomic re-key after a revoke *(signed)*

Request `wire.RotateReq` `{hk_version, hk_wraps:[HKWrap], dek_wraps:[DEKWrap]}`,
applied all-or-nothing in one transaction. Rules:

- `hk_version` must be exactly current + 1;
- `hk_wraps` must be non-empty and every `device_id` in it currently active;
- `dek_wraps` must cover **exactly** the existing set of `key_id`s — no missing,
  extra, or duplicate entries. The error names the first offender.

On success all HK wraps are replaced (devices absent from `hk_wraps` lose
access), all data-key wraps are overwritten under the new HK, and the version is
bumped. Records are untouched. → `200 {"hk_version": N}`. Errors: `400` on any
rule violation.

### Recovery endpoints

`GET /v1/recovery/salt` is open — the salt is an Argon2id input, not a secret,
and is needed before any recovery signature is possible. Everything after it is
signed with the recovery key: `GET /v1/recovery` returns the HK wrap,
`POST /v1/recovery/token` mints an enrollment token, and
`POST /v1/recovery/activate/{id}` admits the replacement machine. Recovery
material is installed by `POST /v1/recovery` at bootstrap and is **write-once**,
so a compromised device cannot swap in a recovery key of its own.

## Multi-tenancy

One server can host several isolated tenants, each with its own devices, keys,
and record streams. **The wire protocol is unchanged** — a client sends nothing
tenant-specific; the server routes each request by the identity that signed it.

- **Device → tenant.** The auth middleware finds the tenant whose `devices`
  bucket holds `X-Yore-Device` (device ids are globally unique ULIDs, so at most
  one can match) and binds that tenant's database into the request context. The
  lookup is cached per device id. Enrollment routes by token instead, and
  recovery by the tenant holding recovery material — with more than one such
  tenant the request is ambiguous and is refused rather than guessed at. No match
  is a `401`. Configured bootstrap tokens are compared constant-time with no
  early break, so a match leaks nothing about which or how many tenants exist.
- **Isolation.** Each tenant is a separate bbolt file, so a query under one token
  can never see another tenant's data. There is no shared fallback database — a
  request that reaches a handler without a resolved tenant fails `500`.
- **One mode or the other.** A server is configured **either** with a single
  token (`--token` / `$YORE_TOKEN` / `$YORE_TOKEN_FILE`), hosting one tenant
  whose database is `--db`, **or** with named tenants (`$YORE_TOKENS_FILE`, a
  JSON `{"name":"token", …}`) at `<dir(--db)>/tenants/<name>.db`, in which case
  nothing is created at `--db` and the path only roots `tenants/` and `backups/`.
  Both together is refused at startup, and so is neither.
- **Names.** `[A-Za-z0-9_-]+`; `default` is reserved, since it names the backup
  directory of a single-token server's tenant. Duplicate tokens are refused at
  startup — two tenants sharing one would be indistinguishable.

## Server storage (bbolt, ciphertext only)

Each tenant's database holds:

- `devices` — device_id → `Device`
- `hk_wraps` — device_id → `HKWrap`
- `dek_wraps` — key_id → `DEKWrap`
- `tokens` — sha256(token) → the token record
- `recovery` — fixed key → `RecoveryInit` (salt, recovery public keys, HK wrap)
- `records:<host_id>`, one bucket per stream — seq (8-byte big-endian) →
  `{id, key_id, blob, created_ms}`
- `meta` — `hk_version`

A host's `max_seq` is the last key of its stream bucket, so there is no separate
index to keep consistent. Exactly one server replica, since bbolt is
single-owner; TLS terminates at your reverse proxy. Optional rolling per-tenant
snapshots land in `<dir(--db)>/backups/<tenant>/data-<unixMillis>.db`
(`$YORE_BACKUP_INTERVAL`, default 1h, `"0"` disables; `$YORE_BACKUP_KEEP`,
default 3).

---

# Part 2 — The crypto scheme (`internal/cryptobox`)

Every confidentiality boundary is **XChaCha20-Poly1305**
(`chacha20poly1305.NewX`) with a fresh 24-byte random nonce; blobs are
`nonce ‖ ciphertext` unless noted. Each construction binds a
**domain-separation string** as additional data (and, for HK, into the HKDF
info), so a blob sealed under one construction can never open under another:
`yore/hk-wrap/v1`, `yore/dek-wrap/v1`, `yore/rec/v1`.

## Device identity — `device.key`

A machine's long-lived identity is two keypairs whose private halves never leave
it: an **X25519** keypair that seals and unwraps the History Key, and an
**Ed25519** keypair that signs sync requests. On disk it is one line, mode
`0600`, and loading is refused if the file is group- or other-readable:

```
yore-device2.<base64url( x25519priv(32) ‖ x25519pub(32) ‖ ed25519seed(32) )>
```

`base64url` is raw and unpadded. The `yore-device2.` prefix marks the format that
added the Ed25519 signing seed; a legacy `yore-device1.` key predates request
signing and must re-enroll. On load, the stored X25519 public key is checked
against the one derived from the private key, rejecting silent corruption.

A machine's **device id is its host id** — the store's stable opaque ULID
(`meta.host_id`) — so no extra persistence is needed and it is distinct per
machine. `pub_key` and `sign_key` on the wire are the 32-byte X25519 and Ed25519
public keys.

## History Key (HK)

One 32-byte random symmetric key per group, created once at bootstrap. It is
**never stored in the clear** — only as per-device wrapped blobs on the server —
and there is no master key. `hk_version` starts at 1 and increments by 1 on each
rotation.

**Wrap** (`WrapHK`) is an anonymous sealed box to the recipient's X25519 public
key: generate a throwaway X25519 keypair, ECDH with the recipient, derive the
wrapping key via HKDF-SHA256, and seal HK. The blob is:

```
ephemeralPub(32) ‖ nonce(24) ‖ ciphertext
```

- HKDF info: `yore/hk-wrap/v1|<b64url(ephemeralPub)>|<b64url(recipientPub)>`
- AEAD additional data: `yore/hk-wrap/v1`

Both public keys are bound into the derivation, so a wrap is cryptographically
tied to the exact (ephemeral, recipient) pair and reveals nothing about the
sender. Only the holder of the recipient's private key can unwrap. X25519
low-order results are rejected.

## Epoch data keys (DEKs)

Records are bucketed into fixed-width **epochs** (default 24h, `key_epoch`).
Epochs tile the timeline from the unix epoch, so every device computes the same
boundary for a given time and width without coordination. The first time a device
records into an epoch it mints a fresh 32-byte data key with a ULID `key_id` and
wraps it once under HK.

**Wrap** (`WrapDEK`) seals the data key under HK as `nonce(24) ‖ ciphertext`,
with AAD:

```
yore/dek-wrap/v1|<keyID>|<deviceID>|<epoch>|<hkVersion>
```

so a wrap cannot be replayed against a different key slot, device, epoch, or HK
generation. `epoch` is unix milliseconds of the epoch start.

## Per-record seal

Each record's plaintext is the JSON of its meaningful fields — `{v:2, id,
host_id, hostname, session, cmd, cwd, exit, dur_ms, start_ms, executor, type,
target_id, prompt_id, prompt, tag_name, tag_desc, tag_op}` — so **everything,
including the hostname, travels encrypted**.

`type` selects what a record *is*:

| `type` | Meaning |
|---|---|
| `""` | a captured command |
| `"delete"` | a tombstone naming its victim in `target_id` |
| `"tag"` | a user-tag add or remove, in `tag_name`/`tag_desc`/`tag_op` |
| `"prompt"` | one user prompt to an agent; `prompt` holds the text, and its `id` is what the commands it caused carry in their `prompt_id` |

So `prompt` is set only on a `"prompt"` record, and prompt text crosses the wire
once per prompt rather than once per command. A client configured with
`sync_prompts = false` never uploads its prompt records; their commands still
sync, and `prompt_id` resolves to no text on other machines.

`executor` (which agent ran a command) and the `tag_*` fields (a user-applied
label) are deliberately separate — see [architecture.md](architecture.md). `v`
was 1 while the executor shared the tag key; pre-1.0, nothing reads the old
shape.

Stream metadata — `seq`, `key_id`, and the outer `id` — rides in the wire record
rather than the ciphertext, and is bound as AAD.

**Seal** (`SealRecord`) under the epoch data key is `nonce(24) ‖ ciphertext`,
with AAD:

```
yore/rec/v1|<recordID>|<hostID>|<seq>|<keyID>
```

so a sealed record cannot be moved to a different id, host stream, sequence
position, or data key without the tamper being detected on open. **Decryption
failure is fatal to a pull** — it signals tampering or a key mismatch and is
returned as an error, never silently skipped.

## Enrollment, verification, revocation, recovery

**Bootstrap** (first device): register self on the server token, mint HK, wrap it
for the recovery key and publish that, wrap it for self, and self-activate (the
server pins `hk_version` 1). Recovery is installed *before* activation: the wrap
can only be made while HK is in its creator's RAM, so if recovery cannot be
established the group must not form at all.

**Enroll** (any later device): `POST /v1/devices` lands pending. The device shows
a **verification code** — six groups of four Crockford-base32 characters over
`SHA-256("yore/verify/v1|" ‖ pubKey)`. An existing active device confirms the
same code out of band, which guards against a swapped key, then approves: it
wraps HK for the newcomer's public key and activates it. The newcomer resolves HK
and can read everything.

**Revoke and rotate**: revoke the device (the server deletes its HK wrap), then
in one atomic `POST /v1/keys/rotate`: mint a fresh HK at `current+1`, **re-wrap
every existing data key** under it preserving each key's plaintext, key id,
device id, and epoch — so old records still open under the same AAD, and
**records are never re-encrypted** — and wrap the new HK for every still-active
device. Rotation is refused if no active device would survive. Unwrapped data-key
plaintexts stay valid across rotation; only the HK-wrap cache is invalidated
client-side.

**What a revoked machine does.** Rotation stops it reading anything *new*, but it
still holds the ciphertext it already pulled and, while its daemon runs, the keys
it already unwrapped. So the client acts on its own revocation: the first
`device_revoked` response records the fact in `data.db`'s meta bucket
(`revoked_by_server` — not a loose file in the state directory, which would be
one `rm` away) and **deletes `remote.db` immediately**, cache detached first so no
later cycle writes it back, pull cursors dropped with it. Sync then stops for the
rest of that process. Remote history already decrypted into RAM is deliberately
left for the rest of the session; it is gone at the next start, which finds an
empty cache and can obtain no key. The persisted record is the durable half: a
start that reads it deletes the cache again before opening anything, covering a
process killed mid-purge, a restored backup, or a machine that comes back up
offline and can never be told.

**The way back.** That record is the last thing the server said, not a permanent
verdict, so a revoked start is not latched and gets exactly one attempt. Nothing
in the enrollment path can clear it directly, since the daemon holds the `data.db`
write lock, so the only evidence that retires a revocation is the server serving
this device again: enroll the machine afresh and the next successful cycle clears
it, while a still-revoked one is refused again and stops. Re-pointing at a
different server clears it too, since the revocation described the old group.

**Recover** (no device survives): the recovery passphrase is stretched with
**Argon2id** (t=3, m=128 MiB, p=4, 16-byte salt) into an X25519 keypair — a
recipient for one extra HK wrap — and an Ed25519 keypair for proof of
possession. `POST /v1/recovery/token` and `POST /v1/recovery/activate/{id}` exist
because the lost machines are still *active* server-side: the bootstrap allowance
does not apply and nothing survives to approve the new device, so possession of
the passphrase is the authorization.

Read cost is constant in history age: one asymmetric HK unwrap per daemon
lifetime, then every data key and record opens symmetrically. Enroll and revoke
re-wrap only keys, never the record corpus.

---

# Part 3 — The daemon unix-socket protocol (`internal/proto`)

**Local only.** This is how the CLI and TUIs drive the background daemon. It
never touches the network and is unrelated to the HTTP API above.

**Transport.** A unix domain socket at `~/.config/yore/daemon.sock`, mode `0600`.
One JSON object per line in each direction; every `Request` gets exactly one
`Response`. A connection may send many requests in order, and the daemon serves
one connection on one goroutine.

**Lifecycle.** The daemon auto-spawns on first use, resets its idle timer on each
request, and exits after `daemon_idle` of silence. A client that finds no daemon
spawns one; the loser of the store lock exits silently.

## Request

```json
{ "op": "query",
  "record": { …rec.Record… },     // OpRecord only
  "query":  { …QueryReq… },       // OpQuery only
  "delete_id": "01J…",            // OpDelete target
  "device_id": "01J…" }           // OpApprove / OpRevoke target
```

`QueryReq`:

| Field | Meaning |
|---|---|
| `q` | query string |
| `scope` | `local` (default) \| `all` \| `host` \| `session` \| `cwd` \| `workspace` |
| `host` / `session` / `cwd` | selector for the corresponding scope |
| `executor` | which agent ran it (e.g. `claude-code`); `""` is any |
| `human_only` | drop everything an agent ran; `hidden_agents` reports how many went |
| `tag` | a user tag the row carries, never an executor name; `""` is any |
| `sort` | `""` for recency (newest first) \| `frecency` (implies dedupe) |
| `fuzzy` | subsequence matching instead of substring |
| `limit` | `0` takes the daemon default (200); `-1` asks for every match |
| `offset` | window offset |
| `dedupe` | collapse identical commands, newest wins |
| `want_prompts` | also return the prompt records covering the window, in `prompts` |
| `prompt_days` | lookback bound for `want_prompts`; `0` is all |

Returned rows always carry their `prompt` text, rejoined from the daemon's prompt
index — the text itself is stored once, on its own record. `want_prompts` exists
for the one thing rows cannot show: a prompt that triggered no commands, and so
appears in no row.

## Ops

| Op | Request fields | Response payload |
|---|---|---|
| `ping` | — | `ok` (resets the idle timer, nudges ingest) |
| `record` | `record` | `ok` (spools and fsyncs, nudges ingest) |
| `query` | `query` | `query` (`QueryResp`) |
| `hosts` | — | `hosts` (`HostsInfo`) |
| `tags` | `tags` (`TagsReq`: `scope`) | `tags` (`TagsInfo`: known tags with per-tag command counts) |
| `delete` | `delete_id` | `ok` |
| `devices` | — | `devices` (`DevicesInfo`) |
| `token` | — | `token` (`TokenInfo`: a single-use enrollment token) |
| `tokens` | — | `tokens_list` (`TokensInfo`: every token and what became of it) |
| `revoketk` | `token_id` | `ok` (cancels an unclaimed token; rotates nothing) |
| `approve` | `device_id` | `ok` |
| `revoke` | `device_id` | `ok` (revoke plus key rotation) |
| `sync` | — | `ok` (runs a full synchronous push/pull cycle) |
| `status` | — | `status` (`StatusResp`) |
| `shutdown` | — | `ok`, then the daemon exits |

## Response

```json
{ "ok": true, "err": "",
  "query":   { "rows":[…], "total":N, "scope":"local", "prompts":[…], "remote":{…} },
  "hosts":   { "hosts":[{"hostname","host_id","count"}], "remote":{…} },
  "devices": { "devices":[{"id","name","status","code","self"}] },
  "status":  { "pid","uptime_sec","local_rows","remote":{…},"version" } }
```

Only the payload for the op is populated; `err` is non-empty on failure, with
`ok` false. `RemoteInfo` is `{state, last_sync_ms, hosts}` where `state` is one of
`off | unavailable | syncing | ok`. Pending devices carry a `code` — the same
verification code as enrollment — and `self` marks this machine.
</content>
</invoke>
