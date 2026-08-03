# DB schema & seed mechanics architecture

PostgreSQL is the system of record. Its schema is defined with Prisma and applied
declaratively with `prisma db push`; its rows are accessed at runtime by
hand-written SQL over `pgx`. The two roles are deliberately split: **Prisma owns
schema authoring and application**, while **the Go services own the request path**
with no ORM in it. This doc covers how the schema is authored, how it is applied
to a database, how PostgreSQL-native objects that Prisma cannot express are
recovered, how a fresh database is seeded in two tiers, and the forward path to
versioned migrations.

Everything lives under `tools/db-migrate/`.

## 1. Schema authoring — the `schema/` folder

The schema source is the `tools/db-migrate/schema/` folder, a Prisma multi-file
schema grouped by domain: `schema.prisma` (datasource + generator only) plus
per-domain files — `identity`, `iam`, `admin`, `providers`, `gateway`, `cache`,
`compliance`, `nodes`, `traffic`, `observability`, `assistant`. Every model and
enum lives in exactly one file; the split is purely organizational — `db push`
from the folder produces the identical physical schema a single file would.

Model and field doc-comments are load-bearing documentation in their own right —
they record intent the SQL alone can't, for example that `Provider.adapterType`
is validated as an enum in the Control Plane handler and the OpenAPI spec rather
than by a SQL `CHECK`, so the value set can evolve without a schema change.

Prisma configuration lives in `tools/db-migrate/prisma.config.ts`: it points
`schema` at the folder, carries the `seed` command, and reads the database URL
from `DATABASE_URL`. The seed entry is expressed as `npm run seed` so a real
shell evaluates the chained command; Prisma 7 otherwise token-splits the string
and would drop everything after the `&&`.

## 2. Applying the schema — `prisma db push`

The schema is applied with `prisma db push` (the `db:push` npm script). `db push`
is declarative: it reconciles the database to the model graph in `schema/`. There
are **no migration files, no baseline, and no `_prisma_migrations` table** in 1.0.
The model graph in `schema/` is the single source of truth for the table shape,
and `db push` makes any database match it.

This is deliberate for the pre-GA / open-source phase: a fresh clone gets a clean
schema in one command with no history to replay, and there is no internal
migration changelog to ship publicly. `dev-start.sh` runs `prisma db push` on
every bring-up (additively by default; `--force-reset` wipes).

## 3. PostgreSQL-native objects — `schema-extras.sql`

`db push` can only create what `schema/`'s model graph expresses. A handful of
PostgreSQL-native objects have no Prisma representation and live in
`tools/db-migrate/schema-extras.sql`, applied **after** `db push` and **before**
seeding. The file is re-runnable (`DROP … IF EXISTS` / `CREATE … IF NOT EXISTS`,
`CREATE OR REPLACE` for the function and view). It contains:

- **`metric_ops_raw` RANGE partitioning** by `sampled_at`, plus its pre-created
  daily partitions. `schema/` declares `MetricOpsRaw` as a plain table; the Hub
  `ops-raw-partition` job requires the table to be partitioned or it fails every
  cycle. (This block drops and recreates the table — ops telemetry is disposable,
  so re-applying on an existing dev database is acceptable.)
- **`cache_key_source(provider_id, key)` function** and the
  **`cache_provider_effective` view**, which resolve and merge the three
  cache-config tiers (global / adapter / provider-override).
- **Partial, expression, and GIN indexes** that Prisma's `@@index` cannot express
  — `WHERE` predicates, expression keys, `COALESCE` expression-unique keys, GIN.
  Several are correctness-bearing, not just performance:
  - `thing_type_physical_id_uniq` — `UNIQUE (type, physical_id) WHERE type='agent'`
    — what keeps an agent's `thing.id` stable across reinstalls (Hub matches the
    hardware fingerprint on re-enroll instead of issuing a new id). See
    [service-call-framework.md](../foundation/service-call-framework.md).
  - `DeviceAssignment_deviceId_active_uidx` — one active assignment per device.
  - `exemption_request_pending_dedup_uniq` — one pending request per (host, requester).
  - `uq_ops_rollup_{5m,1h,1d,1mo}` — `COALESCE(thing_id,'')` expression-unique keys
    that dedup rollup rows.

  The set is recovered by diffing the schema `db push` produces against the schema
  a full apply of the historical migration lineage produced; only objects valid
  under the current `schema/` columns are kept.

### Applying it is all-or-nothing, and must fail loudly

Every caller applies the file with `prisma db execute --file schema-extras.sql`,
which resolves the target from `DATABASE_URL` — the container migrator
(`docker/db-migrator/entrypoint.sh`), `dev-start.sh` and its PowerShell twin
`dev-start.ps1`. The file is applied as **one script that stops at its first
failing statement**, so a single failure is not a single missing object: every
object declared after it is skipped too, and the objects listed above as
correctness-bearing sit near the end of the file.

This makes a swallowed error uniquely expensive, because a partial apply looks
exactly like a successful one — `db push` succeeded, the seed succeeded, the
services start. A developer database was found in that state carrying only 12 of
the file's 41 indexes: two `thing` rows shared one `(type='agent', physical_id)`
pair, `thing_type_physical_id_uniq` could therefore not be created, and the abort
took the remaining 29 indexes with it — including
`exemption_request_pending_dedup_uniq`, whose absence turns the agent exemption
upsert into `no unique or exclusion constraint matching the ON CONFLICT
specification`. The bring-up had reported success every time because the apply
step discarded both streams and only warned.

Two rules follow, and both are load-bearing:

- **No caller may downgrade an apply failure to a warning.** The error text names
  the first statement that aborted; that name is the whole diagnosis.
- **A data conflict that blocks a unique index is remedied before the apply, not
  by loosening the index.** The remedy is a one-off script under
  `manual-scripts/` — for the case above,
  `clear_duplicate_thing_physical_id_2026_08_03.sql`, which clears `physical_id`
  on the superseded duplicates rather than deleting rows (the index predicate
  excludes NULL, and `thing` is the parent of several `ON DELETE CASCADE`
  children).

## 4. Seeding a fresh database — three tiers

`tools/db-migrate/seed/seed.ts` brings a freshly-pushed database to a usable state
through the Prisma Client (with the `@prisma/adapter-pg` driver adapter). It runs
**Tier A (reference) always**, then **bootstrap (minimal tenant) always**, then
**Tier B (demo playground) unless `SEED_DEMO=false`**. Splitting the always-on
minimal tenant out of the demo playground is what lets a production install
(`SEED_DEMO=false`) ship a loginable admin and a working assistant **without any
demo rows** — so no repo-committed credential is usable on an internet-facing
deployment.

### Tier A — reference catalog (always)

The product reference data every install needs: providers, the model catalog
(with pricing and capability columns), the interception domain/path catalog,
compliance rules and rule-packs, config templates, the managed IAM policies and
the standard IAM groups + their policy attachments, `system_metadata` defaults,
the cache-config singletons, and the `semantic_cache_config` row.

It is delivered as versioned JSON fixtures under `seed/fixtures/<table>.json`,
loaded by idempotent `upsert` keyed on each table's unique key
(`seed/reference/`). The fixtures are produced by
`scripts/extract-reference-fixtures.ts`, which selects each reference table from a
source database with `row_to_json` (lossless — it captures every column, unlike a
`pg_dump` short-form `INSERT`) and applies a curation pass that excludes
operational rows (e.g. a runtime version counter, any real secret) and normalizes
captured-drift values back to product defaults. The 5 rule-pack YAMLs stay under
`seed/rule-packs/`.

#### Fields the deployment owns

The fixture replaces every field of a row it matches — that is what makes the
seed the source of truth for a catalog fact. It is exactly wrong for a value
only the install can know, and one such value exists today:
`OAuthClient.redirectUris` for `cp-ui`. Every deployment answers on its own
domain, so its console callback URL is deployment configuration; the fixture
ships the `localhost` URLs a developer needs and has no way to know the rest.

Such fields are declared in `UNION_FIXTURE_FIELDS` and **merged, not replaced**:
the seed guarantees its own values are present and removes nothing it did not
ship. Retracting a value is the admin API's job, which is where the rest of a
client's runtime state already lives.

The asymmetry is deliberate. The seed cannot tell a URL an admin registered on
purpose from one left over, and the two mistakes are not equal: re-adding a URL
an admin removed is a nuisance, while deleting one they still need locks every
admin out of the console — the authorize endpoint rejects an unregistered
`redirect_uri`, so the failure is total, and it arrives whenever someone
re-seeds for an entirely unrelated reason. A replaced `redirectUris` took a
production console down for eleven hours before anyone noticed.

#### Model identity and model references

`Model` is not a plain keyed upsert. A live database can already hold a catalog
model under an id the seed has never seen — an admin added it through the provider
wizard, or the catalog renamed a code the row still carries — so the fixture's id
and its code can each independently match a different row, and a single-key upsert
falls through to `create` and dies on the other key's unique constraint. The seed
therefore **reconciles** each model: it matches on the fixture's id OR its code OR
any of its aliases OR `(providerId, providerModelId)`, and on a match updates the
live row **in place, keeping that row's id**. Rewriting the id instead would
silently orphan `traffic_event.model_id`, `VirtualKey.allowedModels[].modelId` and
every routing reference — all plain columns with no foreign key, so nothing would
raise.

The identity list is **every unique key the table declares besides `id`**, and
that is a rule, not a coincidence: a key left out is still enforced by the
database at create time, so omitting one converts an adoptable row into a failed
seed. `(providerId, providerModelId)` is the one most easily forgotten and the
one the wizard reaches — the admin API exposes Code and ProviderModelID
independently, so a hand-added `my-gpt-5.6` against `gpt-5.6` matches the fixture
on neither id nor code. A composite key is one identity: matching its columns as
alternatives would adopt an unrelated sibling. The same rule gives `rule` its
`(packId, ruleId)` key.

The consequence is that no other fixture may hardcode a model id: after an
adoption the fixture's id names nothing. Fixtures that depend on a model
(`ai_guard_config.model_id`, `RoutingRule`'s `config` / `matchConditions` /
`fallbackChain`, and demo `VirtualKey.allowedModels`) instead name it by code as
`model:<code>`. Once the catalog has been seeded, the seed reads the model ids back
from the database and resolves every such reference to the id the row actually
holds, so a dependent row follows the surviving model whatever its id. A reference
naming a code the catalog does not ship fails the seed rather than writing a
dangling id, and the extractor emits these references instead of capturing the
source database's ids.

The read-back happens **only when `Model` itself seeded**. A failed `Model` leaves
the table in whatever state the failure stopped at, and resolving references
against that would point dependent rows at rows the run never vouched for —
silently, since none of these columns is a foreign key. So the ids stay unknown
instead, and every fixture that names a model fails on the reference it could not
resolve. Those failures are collected like any other and named in the aggregate
report: the seed was already failing, and this decides only whether it fails
honestly or writes ids it cannot stand behind.

### Bootstrap — minimal tenant (always)

The smallest tenant every install needs to be usable: one organization, one
project, the super-admin (`admin@nexus.ai`), the super-admin's IAM binding
(membership + direct policy attachment — the group and policy themselves are
Tier-A rows), and a dedicated **system-assistant** virtual key that powers
Chat-with-Nexus. Delivered as fixtures under `seed/fixtures/bootstrap/`
(`seed/bootstrap/`) with secret columns nulled. At seed time `seedBootstrap`
re-stamps the two secrets under the local keys: the super-admin password via
scrypt `hashPassword` (matching the Control Plane verifier) and the assistant VK
hash via HMAC `hashVirtualKey` (under `ADMIN_KEY_HMAC_SECRET`), using a
deterministic local plaintext (`assistantVkKey`). It requires
`ADMIN_KEY_HMAC_SECRET` and fails fast if absent. These deterministic dev
defaults are PUBLIC in the OSS repo; the appliance overwrites both with
per-instance random secrets at first boot (see §AMI below).

### Tier B — demo playground (opt-out)

A rich, navigable demo so an evaluation install is immediately explorable: a
multi-org tenant, additional demo users, a demo provider + credential, demo
models, quota policies, and virtual keys. It is delivered as fixtures under
`seed/fixtures/demo/` (`seed/demo/`), with all source-system secret columns nulled
in the committed files. Demo rows owned by the super-admin (virtual keys, admin
keys) resolve because the bootstrap tier — which seeds the super-admin — always
runs first. At seed time `seedDemo` **re-stamps** the secrets under the local
keys: passwords via scrypt `hashPassword`, virtual / admin key hashes via HMAC
(under `ADMIN_KEY_HMAC_SECRET`), and credential ciphertext via AES-256-GCM
`fakeEncrypt` (under `CREDENTIAL_ENCRYPTION_KEY`). The documented demo credentials
are printed in a banner at the end of the seed. Tier B therefore requires both env
keys; it fails fast if either is absent.

### Bootstrap flow

- Local / CI: `prisma db push` → apply `schema-extras.sql` → `prisma db seed`
  (Tier A + bootstrap + Tier B). `dev-start.sh` does exactly this, and aborts the
  bring-up if the extras apply fails (see §3) instead of continuing into the seed
  against a half-built schema.
- Production / clean install (incl. the AMI): `prisma db push` → apply
  `schema-extras.sql` → `npm run seed:prod` (`SEED_DEMO=false` — Tier A +
  bootstrap only, no demo rows).

All seed writes are idempotent upserts: re-running converges, never duplicates.

## 5. Forward path to versioned migrations

`db push` has no migration history, so it cannot perform a controlled, reviewable
production schema upgrade. When the first post-1.0 schema change needs that, the
project reintroduces `prisma migrate`, generating the first migration **from the
1.0 `schema/`** as the new starting point. 1.0 itself ships pure schema + `db
push` + `schema-extras.sql`.

## 6. Out-of-band scripts

`tools/db-migrate/manual-scripts/` holds one-off SQL — data backfills, cost
recomputes — that an operator runs by hand against a specific environment. These
are data operations run deliberately with operator judgment, distinct from the
declarative schema (`schema/` + `schema-extras.sql`) and from the seed.

One category is a **precondition** rather than a backfill: data that blocks a
`schema-extras.sql` unique index has to be resolved before the file can apply, so
the script runs first and the apply runs second
(`clear_duplicate_thing_physical_id_2026_08_03.sql`). These are the scripts to
reach for when an apply aborts, and the reason the apply must surface its error
rather than warn.

## 7. Runtime access and the Go config mirrors

At runtime the Go services read and write through `pgx` with hand-written SQL —
there is no ORM in the request path (for example
`packages/ai-gateway/internal/platform/store/pgx.go` and the cache layer's
loaders). Prisma is a build- and deploy-time tool, not a runtime dependency.

The schema carries several JSON columns whose shapes are configuration domains
(the `thing_config_template` config-key payloads). Go consumers need typed access
to those shapes, so `packages/shared/schemas/configtypes/` hand-maintains the
mirror — five sub-packages (`enums`, `identity`, `interception`, `observability`,
`policy`) of Go structs matching the JSON shapes. There is no generator from the
schema to these structs; they are kept in step by hand, and the configuration
architecture's per-key catalog is the registry that keeps the two sides aligned.

## 8. Renames

A rename that touches a schema-adjacent identifier (a column, a config key, an env
variable) has to be swept across far more than the schema. `scripts/check-rename.sh`
is the sweep gate that verifies a rename reached every layer — Go source and
tests, YAML and env examples, the schema (`schema/` + `schema-extras.sql`), the
seed fixtures, UI source and i18n, production systemd and database rows, docs,
skills, and the agent rule files. The binding rule and the full layer list live in
[configuration-architecture.md](../foundation/configuration-architecture.md).

## References

- `tools/db-migrate/schema/` — multi-file schema source (single source of truth)
- `tools/db-migrate/prisma.config.ts` — Prisma configuration (`schema` folder + seed)
- `tools/db-migrate/package.json` — `db:push` / `generate` / `seed` / `seed:prod` scripts
- `tools/db-migrate/schema-extras.sql` — post-push PostgreSQL-native objects
- `tools/db-migrate/seed/seed.ts` — three-tier seed entry (`shouldSeedDemo` switch)
- `tools/db-migrate/seed/reference/` + `seed/fixtures/` — Tier-A reference catalog
- `tools/db-migrate/seed/bootstrap/` + `seed/fixtures/bootstrap/` — minimal tenant (always)
- `tools/db-migrate/seed/demo/` + `seed/fixtures/demo/` — Tier-B demo playground
- `tools/db-migrate/scripts/extract-reference-fixtures.ts` — fixture generator
- `tools/db-migrate/manual-scripts/` — out-of-band data scripts
- `scripts/check-rename.sh` — rename sweep gate
- `packages/shared/schemas/configtypes/` — hand-maintained Go config-shape mirrors
