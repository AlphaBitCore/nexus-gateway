# E42-S4 — Universal `log_level` Shadow Key (SDD)

## User story

As an SRE during an incident, I bump a service's log level to `debug` from
the Control Plane UI and the running binary starts emitting verbose logs
within a second — no restart, no SSH, no YAML edit. When the incident is
over I flip it back to `info` and the verbose noise stops just as
quickly.

## Background

Every Nexus Gateway service has a `log.level` YAML field that is read
once at startup and baked into the `slog.Handler` via
`slog.HandlerOptions.Level`. To change verbosity today operators must
edit the YAML and restart the binary, which a) drops in-flight requests
and b) is impractical for the Hub which runs leader-elected jobs.

`slog` already has the building block we need: `slog.LevelVar` is a
mutable `slog.Leveler` that supports atomic `Set()`. The
`HandlerOptions.Level` field accepts any `Leveler`, so plugging a
`LevelVar` in once at process start gives every subsequent log record
a hot-swap entry point at zero performance cost on the hot path.

## Tasks

1. **`shared/logging` refactor**:
   - Add a package-level `currentLevel slog.LevelVar` whose initial
     value is set inside `NewLogger` from `cfg.Level`.
   - Change `slog.HandlerOptions.Level` from the captured `level`
     constant to `&currentLevel` so the handler reads the level via
     the leveler on every record.
   - Export `SetLevel(name string)` that calls
     `currentLevel.Set(ParseLevel(name))`. ParseLevel's existing
     "unknown → Info" fallback is retained so a misspelled shadow
     payload degrades rather than crashes.
   - Export `CurrentLevel() slog.Level` (returns `currentLevel.Level()`)
     so handlers can confirm the apply landed.
2. **Shadow handler in each consumer service** (`ai-gateway`,
   `compliance-proxy`, `agent`, `control-plane`):
   - Add a `case "log_level":` (for ai-gateway / cp / control-plane in
     their `thingclient.Options.OnConfigChanged` switch; for agent in
     `configsync.ShadowApplier` dispatch table) that unmarshals
     `{"level": "..."}` and calls `logging.SetLevel(...)`.
   - Log the transition at `slog.LevelInfo` so even when the new level
     hides further DEBUG logs, the operator can confirm the apply via
     the access log.
3. **Hub self-shadow handler**:
   - Register a second handler on the existing `selfshadow.Manager`
     (added in PR1) for `log_level`, invoking the same
     `logging.SetLevel`.
4. **Migration**:
   - Add 5 rows to `thing_config_template` (one per type) with
     default state `{"level": "info"}`.
   - Backfill existing Things' `desired` to include the key with the
     same default if absent (per the established pattern in
     `20260514000000_e42_config_template_audit`).
5. **Tests**:
   - `shared/logging`: a unit test that asserts SetLevel toggles the
     visible record set (Debug suppressed at Info, visible after
     SetLevel("debug")).
   - Hub `selfshadow`: extend the existing manager tests to register
     a stub handler for `log_level` and verify dispatch.

## Non-tasks (explicitly out of scope)

- Per-component log filters (e.g. set ai-gateway's `routing` package to
  debug while leaving everything else at info). Possible later via
  per-package LevelVars, but no operator request for it yet.
- Audit trail of who flipped the level when. The existing
  `config_change_event` chain wired by Hub's override path covers it
  automatically.

## Acceptance criteria

- [ ] `go test ./packages/shared/runtime/logging/... -race -count=1` passes,
      including the new SetLevel toggle test.
- [ ] Setting an override on `<service>/log_level` from the Control
      Plane admin UI causes the matching binary to emit DEBUG records
      within 2 seconds, observed in its log file.
- [ ] Clearing the override returns the service to its YAML-defined
      level on the next NOTIFY / reconnect dispatch.
- [ ] `SELECT type, config_key FROM thing_config_template WHERE config_key = 'log_level';`
      returns 5 rows.
