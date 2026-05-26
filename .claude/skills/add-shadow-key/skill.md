# add-shadow-key

Walk the three-path audit when adding or changing a Thing shadow key (Cat A / B / C).

Use this skill whenever you:

- Add a new entry to `thing_config_template`.
- Change a key's category (A ↔ B ↔ C).
- Change a key's schema in a way the apply path must understand.

This procedure exists because **three separate paths create or modify `thing_config_template` rows**, and they can drift independently. Auditing only one path produces false positives. Memory: `feedback_thing_config_template_audit_paths`.

---

## The three paths

1. **`configreconcile`** — Hub runtime path that creates / updates Thing rows on registration. `packages/shared/configreconcile/`.
2. **`tools/db-migrate/seed/seed.ts`** — canonical Prisma seed (dev DB + prod-data baseline).
3. **`tools/db-migrate/prisma/migrations/**`** — durable schema migrations.

All three must agree.

---

## The 6-step audit

### 1. Pick the category (Cat A / B / C)

| Category | When | Wire shape |
|---|---|---|
| **Cat A — inline** | Small fast-path key (kill switch toggle) | Full value in shadow JSON |
| **Cat B — pull-on-signal** | Mid-to-large key (hook config, routing rules) | `{ version, needsPull: true }` |
| **Cat C — template-fallback** | Template default with per-Thing override | Template + override |

### 2. Cat B keys MUST carry `needsPull: true` (binding)

Memory: `feedback_thing_config_pull_model` + the #91 prod incident (agent.desired missing 4 registered Cat B keys → agents never pulled them).

When you register a Cat B key, verify `needsPull: true` is in the template definition.

### 3. Update `configreconcile`

If the new key has a runtime default, add it to the reconcile path so freshly registered Things receive it.

```go
// packages/shared/configreconcile/...
template[<key>] = configtypes.TemplateEntry{ Category: "B", NeedsPull: true, ... }
```

### 4. Update `tools/db-migrate/seed/seed.ts`

Add the same key to the canonical seed so dev DBs + prod-data baselines start with it.

```ts
// tools/db-migrate/seed/seed.ts
// inside thing_config_template seed block:
{ thingKind, key: '<key>', category: 'B', needsPull: true, defaultValue: ..., ... }
```

### 5. Write the migration

```bash
cd tools/db-migrate
npx prisma migrate dev --name add_thing_config_template_<key>
```

Then verify uniqueness:

```bash
npm run check:migration-timestamps
```

### 6. OnConfigChanged callback

Each Thing that cares about the new key must register an `OnConfigChanged` callback in its `main.go`. The callback validates + atomic-pointer-swaps the in-memory snapshot, then stamps the reported state.

```go
client.OnConfigChanged("<key>", func(ctx context.Context, raw json.RawMessage) error {
    var parsed YourSchema
    if err := json.Unmarshal(raw, &parsed); err != nil { return err }
    snap.Store(&parsed)
    return nil
})
```

Without this, the change-signal arrives but nothing happens locally — silent staleness.

---

## Verification before merge

```bash
# Verify all three paths reference the key:
grep -n '<key>' packages/shared/configreconcile/   # path 1
grep -n '<key>' tools/db-migrate/seed/seed.ts      # path 2
ls tools/db-migrate/prisma/migrations/ | grep <key> # path 3

# Verify the migration is unique:
npm run check:migration-timestamps

# Run the Hub locally; register a fresh Thing; observe the shadow on first pull:
cd packages/nexus-hub && go run ./cmd/nexus-hub/ &
# (then in another shell) start your service; check Hub logs for the shadow contents.
```

---

## Output (PR description)

```
Shadow key audit:
- Key: <key>
- Category: A | B | C
- Cat B needsPull: TRUE (or N/A)
- Three paths in sync:
   * configreconcile: ✓
   * seed.ts:         ✓
   * migration:       ✓ (timestamp unique)
- OnConfigChanged callback wired in:
   * <service-1>/cmd/.../main.go: ✓
   * <service-2>/cmd/.../main.go: ✓
- Verification: registered a fresh Thing locally; pull observed the new key.
```

If any path is missing, **stop and fix before merging**. The #91 incident was exactly this — Cat B keys registered but missing `needsPull` reached prod and agents quietly drifted.
