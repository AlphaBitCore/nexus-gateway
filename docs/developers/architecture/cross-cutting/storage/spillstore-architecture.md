# Spillstore architecture

The spillstore is where the audit pipeline keeps captured request/response bodies
that are too large to carry inline in PostgreSQL. AI workloads make this routine:
a single large-context request or a long streamed response can reach several MiB,
far past what belongs in a hot-path JSONB column. The spillstore is a small,
pluggable abstraction — local filesystem or S3 — that holds those bytes
out-of-band, leaving the audit row with only a compact reference.

How bodies are captured and emitted into the audit pipeline is covered in
[audit-pipeline-architecture.md](../observability/audit-pipeline-architecture.md);
this doc covers the store itself — the interface, the backends, the upload path,
and retention.

## 1. Two-tier body storage

Every captured body takes one of two paths, decided by size:

- **Inline** — bodies below the cutoff travel on `traffic_event_payload` as JSONB
  (base64-encoded on the wire). Fast to write and to read back for the admin UI.
- **Spilled** — bodies at or above the cutoff are written to a `SpillStore`
  backend, and the audit row carries only a `SpillRef` (backend name, storage key,
  SHA-256). The bytes never touch the database.

The cutoff is `MaxInlineBodyBytes` on the payload-capture config (default 256 KiB,
admin-tunable through the payload_capture shadow). It is deliberately *not* a
spillstore setting: the inline-vs-spill threshold is an admin concern, while the
spillstore owns only where spilled bytes land and how long they live. The emit
helper (`packages/shared/storage/spillstore/emit.go`) applies the rule: with no
store configured, or a body below the cutoff, it emits inline; otherwise it writes
to the store and emits a spill reference.

When a `Put` fails, the helper falls back to inline rather than dropping the audit
row — but the fallback is **bounded to the same cutoff an inline body is already
allowed to carry**, and the resulting body is marked `Truncated` while `SizeBytes`
keeps reporting the body's real size. So the record says *this body was N bytes,
here is the prefix we could keep*. An unbounded fallback defeats itself: a payload
large enough to need spilling becomes a publish larger than the MQ `max_payload`,
and the row is lost anyway — loudly, elsewhere, after the memory has been spent
several times over.

Note the asymmetry: that bound applies to a **failed** spill. With **no** spill
backend configured at all, an oversize body is still carried inline whole,
because keeping bodies inline is then a deliberate configuration rather than a
malfunction. Bounding that case too would start truncating audit bodies that are
stored complete today, so it is a data-completeness decision rather than a bug fix.

## 2. The `SpillStore` interface

`packages/shared/storage/spillstore/spillstore.go` defines the cross-service
contract, intentionally minimal so a new backend is a drop-in:

- `Put(content, size, opts)` — stores the bytes and returns a `SpillRef`. Every
  backend hashes the content with SHA-256 and stamps the hex digest onto the ref.
- `Get(ref)` — opens a reader over a stored object (`ErrNotFound` when the ref no
  longer resolves, which callers treat as "already gone").
- `Delete(ref)` — removes an object.
- `Sweep(olderThan)` — deletes objects past the retention horizon, oldest first,
  and may also enforce a total-size ceiling.
- `Stat()` — backend name, object count, total bytes, oldest/newest timestamps,
  for admin and metrics.
- `Backend()` — the canonical backend name stamped onto every `SpillRef`.

A backend may additionally implement the optional `Presigner` capability
(`PresignPut` + `KeyFor`). The S3 backend does; the localfs backend does not (it
returns `ErrPresignNotSupported`). The Hub's upload-mint endpoint type-asserts the
store to `Presigner` to decide between handing back an S3 URL and falling back to
its own in-Hub upload sink.

## 3. Backends and the factory

`packages/shared/storage/spillstore/spillfactory` builds a store from the
per-service YAML `spill:` block. Its `FactoryConfig` is the operator-facing
configuration:

- `enabled` — gates the whole subsystem. When false the factory returns no store
  and every body stays inline regardless of size.
- `backend` — `localfs` (default) or `s3`.
- `localfs` / `s3` — backend-specific options: storage location, per-object cap,
  total-size cap, retention days.
- `async` — wraps the backend so `Put` returns as soon as the key, hash, and size
  are known, with the actual upload running on a background worker.

### localfs

The reference backend writes objects under a configured root directory. All
services in one deployment that share a localfs store must point at the same root
(a shared volume) so any service's spilled bytes are readable by the reader path.

### s3

The S3 backend stores each object at `<prefix>/<date>/<event-id>-<direction>.bin`
— the same date-prefixed layout localfs uses, so retention sweeps work the same
way on both. It signs the SHA-256 checksum into uploads so S3 rejects a body that
does not match. Credentials come from the AWS SDK default chain (IAM role,
environment, or profile); access keys are never plumbed through YAML. The backend
also works against S3-compatible stores (MinIO, Ceph, R2) via a custom endpoint
and path-style addressing.

### async wrapper

For S3, where a `PutObject` round-trip can add hundreds of milliseconds to a
request, the async wrapper moves the upload off the hot path: `Put` computes the
ref synchronously and queues the bytes for a background worker. The trade-off is
durability — queued-but-not-yet-uploaded bodies are held in memory and lost on a
crash, leaving a `SpillRef` that points at an object that never landed. A later
read of that ref returns not-found, which matches the at-most-once guarantee the
audit pipeline already makes. Services should close the store on shutdown to drain
the queue.

### Is a backend actually configured? — the `storage.spill` runtime source

`enabled` defaults to **false**, and the factory returns no store when it is off.
Everything downstream then keeps bodies inline — including when an admin has set an
inline-vs-spill threshold. That control cannot see the backend it depends on, so it
reads as configured while doing nothing. (A second control, a per-host raw-body-spill
switch, was removed outright: nothing ever read it and every stored value was NULL.) That is not hypothetical: the shipped `*.dev.yaml` files enable spill,
and none of the prod-shaped `*.config.yaml` files carry a `spill:` block at all.

Two signals close that gap, and neither adds an admin surface:

**And how much is already there — `residency`.** The same source now carries a bounded
measurement of the backend's current contents: object count, total bytes, and the oldest /
newest object timestamps. It is measured **when the source is read**, not at boot, because
that number moves while the process runs.

Three properties make it safe to be the first consumer of `SpillStore.Stat()`, which
previously had none on a shipped interface precisely because it was not safe:

- **Bounded.** `localfs` stops at 50 000 objects and `s3` at 10 list pages; past the bound
  `truncated: true` and `scanLimit` are reported, so a lower bound never reads as a total.
  The s3 bound was already there and was **silent** — it returned the partial numbers
  unlabelled, which is the same defect shape as a silent drop.
- **Interruptible.** `localfs.Stat` now checks its context per entry (not per directory — one
  flat day-directory with a million files would otherwise never reach a cancellation point),
  and a cancelled scan returns both the partial numbers AND `ctx.Err()`, so a timeout reads as
  a timeout rather than as an almost-empty store.
- **Non-fatal.** A measurement failure leaves `residency` **absent** rather than zero-valued
  and puts the reason on `effect`. "We could not look" and "the store is empty" are different
  answers, and a zero count reads as the second when it means the first.

- **At boot**, the factory logs its posture on every path. The two enabled paths
  already announced themselves; the disabled path now does too, naming the
  consequence rather than only the state.
- **At runtime**, the AI Gateway and the Compliance Proxy — the two services that
  capture bodies, and therefore the two where the threshold applies — register a
  `storage.spill` source on their existing runtime-introspection registry. It reaches
  an admin through the surface that already exists: `GET /debug/runtime` → the Hub
  bridge → `GET /api/admin/nodes/:id/runtime` → the node detail page's Runtime State
  tab, which renders sources generically. No new endpoint, no new page, no new IAM
  action.

`spillfactory.Describe` builds the payload from the boot config and the store the
factory actually returned. It reports whether a backend exists, which one, **where**
it stores, whether that location is host-local, whether writes are async, and the
consequence in plain language. Location and host-locality are there for a specific
failure: two nodes each running their own localfs root write successfully and read
each other's objects never, which is only visible by comparing roots across nodes —
otherwise it surfaces much later as a `not_found_host_local` read failure (section 5).

It deliberately does not call `Stat()`. `Stat` is the natural source of object counts,
but the localfs implementation is an unbounded `filepath.Walk` that ignores its
context, so calling it from an admin endpoint would trade a diagnostic for a stall on
a large spool.

### Per-object cap

Each backend enforces a hard per-object ceiling (256 MiB by default). The
producer-side streaming capture also reads this cap to bound in-memory growth on
long streamed responses, independent of the inline-vs-spill cutoff.

## 4. The upload path

A service that runs its own backend — the AI Gateway and Compliance Proxy — writes
spilled bodies directly to that backend. The agent cannot: it sits behind NAT with
no Hub-reachable storage, so it spills through the Hub. The agent keeps a local
localfs store for oversize bodies and pushes them to the Hub via a two-step mint
and upload:

1. **Mint** — `POST /api/internal/things/spill-uploads`, authenticated by the
   agent's mTLS thing identity. The agent sends the event id, direction, size,
   content type, and the body's SHA-256. The Hub validates the request (including
   the per-object cap, which rejects oversize bodies with 413), derives the storage
   key, and signs a one-shot HMAC upload token. **The key is namespaced by the
   authenticated node identity** — `<nodeId>/<day>/<eventId>-<direction>.bin` for a
   device caller — and the `nodeId` plus the exact key are bound into the signed
   token (SEC-M5-01). It then returns either an S3 presigned URL (when the backend
   is S3) or an in-Hub blob URL carrying the token (when the backend is localfs).
2. **Upload** — for S3 the agent `PUT`s the bytes straight to the presigned URL.
   For localfs the agent `PUT`s to `/api/internal/spill/blob/:token`, a Hub sink
   that authorizes on the HMAC token alone (the mTLS identity was already verified
   at mint), requires the `Content-Length` to match the token's size, enforces
   one-shot use with a Redis dedup key (a replayed token gets 409), streams the
   body into **the exact node-namespaced key the token signed** (the localfs store
   honours `PutOptions.Key` rather than re-deriving a shared key — SEC-M5-01) while
   recomputing the SHA-256, and rejects (and deletes) a body whose hash does not
   match the token.

**Cross-node tamper resistance (SEC-M5-01).** Because the storage key is
node-namespaced and HMAC-bound, one node can never address — let alone overwrite —
another node's spill object: node A minting for node B's `eventId` produces a key
under `A/…`, which is orphaned (B's `traffic_event` references B's key). Direct
in-process spillers (ai-gateway / compliance-proxy, holding the high-trust service
token) keep the flat key. **On read**, the Control Plane's `resolveSpillBody`
recomputes the SHA-256 of the fetched bytes and refuses to serve a body whose hash
does not match the `sha256` recorded on the `traffic_event`, so even a tampered
at-rest blob can never be presented as the genuine captured request/response.

The Hub never decides *whether* to spill — that is the data plane's call. The
upload API is pure infrastructure: token minting and a token-gated sink.

## 5. Reading a spilled body back

The Control Plane is the only reader of spilled traffic bodies. It resolves them
in two places — the traffic drawer (`resolveSpillBody`, which shapes the bytes for
the UI) and the view-time normalize recompute (`rawSpillBody`, which needs the
captured bytes verbatim so spilled SSE is not mis-detected as a quoted JSON
string). Both go through one fetch helper, so the integrity gate and the failure
diagnosis exist once.

A body that cannot be fetched is never an endpoint error: that direction degrades
to empty and the row still renders. The log is therefore the only place the reason
appears, and it carries a stable `cause` label plus an operator-facing `remedy`,
alongside `stage` (`view` or `normalize`), `direction`, and both backend names.

| `cause` | What happened | What the operator does |
|---|---|---|
| `not_found_host_local` | A `localfs` ref, and the object is not on this host | **Structural.** A localfs ref is only readable on the host that wrote it. Either mount the same root on every node or move to a shared backend (s3). Retention may also have swept it |
| `not_found` | Absent from a shared backend | Swept by retention, or an async upload was queued and lost before it landed |
| `backend_mismatch` | The ref names a different backend than this node is configured with | Refs written before a backend change are not readable through the new one |
| `transport` | The backend could not be reached | Connectivity, credentials, permissions. The bytes are probably still there |
| `integrity` | The bytes do not match the recorded SHA-256 | A security event — the blob was refused, not served. See the tamper note in section 4 |
| `read` | The object opened, then the stream broke | A transfer fault against a live backend |
| `ref_decode` | The row's `spill_ref` column is not a decodable ref | A defect in the row, not in storage |

The first cause is the one the labels exist for. Section 3 states that all services
sharing a localfs store must point at the same root; nothing enforced it, so a
deployment that violated it made *every* spilled body permanently unreadable from
the Control Plane while logging the same sentence a transient S3 hiccup produces.
Distinguishing "unreachable by construction" from "unfetchable right now" is the
difference between fixing the deployment and waiting for a retry that will never
succeed.

## 6. Retention

Each backend bounds its own footprint with three controls, all set per backend in
the `spill:` config block. The per-object cap (256 MiB default) bounds any single
write — the producer-side capture clips at the cap, and the agent upload mint
rejects an over-cap body outright. The total-size cap (50 GiB localfs / 10 GiB S3
default) and the retention horizon (30-day default) are enforced by `Sweep`, which
deletes objects past the horizon and evicts oldest-first to hold the total under
the cap.

Because a `SpillStore` is process-local, each service that owns one runs its own
periodic sweep (`packages/shared/storage/spillstore/spillsweep`): the loop calls
`Sweep` on startup and then on an interval, passing `now − RetentionHorizon`. A
localfs store is swept by the process that owns its directory; a shared S3 bucket
is swept by every process pointed at it, which is safe because `Sweep` is
idempotent. The retention horizon comes from the backend's `retentionDays`
(defaulting to 30 days); the agent's fixed local store uses the same default. This
runs alongside, not instead of, any backend-native lifecycle (an S3 bucket
lifecycle rule remains a fine belt-and-suspenders for age-based expiry).

## 7. Configuration ownership

Two configs govern the spill subsystem, split by audience:

- **Operator** — the `spill:` YAML block (`FactoryConfig`): which backend, where it
  stores, the caps, retention, and async. These are deployment concerns.
- **Admin** — `MaxInlineBodyBytes` on the payload_capture shadow: the inline-vs-spill
  cutoff. This is a runtime policy concern, tunable without a redeploy.

Keeping the cutoff out of the backend config means an admin can change how much
body travels inline without touching where spilled bytes are stored.

## References

- `packages/shared/storage/spillstore/spillstore.go` — `SpillStore` + `Presigner` interfaces, `SpillRef`
- `packages/shared/storage/spillstore/emit.go` — inline-vs-spill emit helper
- `packages/shared/storage/spillstore/spillfactory/factory.go` — `FactoryConfig` + backend construction
- `packages/shared/storage/spillstore/spillfactory/availability.go` — the `storage.spill` runtime posture
- `packages/shared/storage/spillstore/localfs/` — localfs backend
- `packages/shared/storage/spillstore/s3/` — S3 backend + presign
- `packages/shared/storage/spillstore/async/` — async upload wrapper
- `packages/shared/storage/spillstore/spillsweep/` — per-service periodic sweep loop
- `packages/shared/audit/body.go` — `Body` / `SpillRef` shapes
- `packages/control-plane/internal/traffic/handler/traffic/traffic_spill.go` — the shared fetch + integrity gate
- `packages/control-plane/internal/traffic/handler/traffic/spill_diag.go` — read-failure cause classification
- `packages/nexus-hub/internal/traffic/ingest/spill/spill_uploads.go` — agent mint + blob upload endpoints
- `packages/agent/cmd/agent/wiring/bridgedeps.go` — agent local spill store wiring
