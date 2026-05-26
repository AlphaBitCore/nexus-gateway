# Activity page — Agent UI feature doc

## Purpose

A local list of agent **lifecycle events** — daemon startup / shutdown, protection pause, SSO login, restart, and similar operational milestones. This is not a traffic-event log; raw traffic events live in the Traffic / Stats surfaces (Wails calls `QueryEvents`, separate from `QueryLifecycleEvents`).

## Columns

The page (`packages/agent/ui/frontend/src/pages/activity/Activity.tsx`) renders three columns:

| Column | Source field |
|---|---|
| Time | `occurredAt` formatted in the active i18n locale (`fmtTime`) |
| Action | the lifecycle `action` rendered through a localised badge (`activity.action.<key>`) |
| Details | a human-readable string built from the event's `attrs` (`fmtDetails`) — shutdown reasons, pause durations, SSO email, etc. |

## Filters

The current page has no filter controls. Lifecycle events are paged (`PAGE_SIZE`-at-a-time via `Previous` / `Next`); the underlying `QueryLifecycleEvents` accepts only `offset` + `limit`.

## Wails bridge

`agentApi.queryLifecycle({ offset, limit })` (`packages/agent/ui/frontend/src/api/agent.ts:417`) calls into the Wails bridge `QueryLifecycleEvents` returning a `LifecycleEventPage`.

## Terminology

- "Traffic event" refers to AI / proxy traffic captured by the agent's forwarder; "lifecycle event" refers to the agent's own operational milestones surfaced here.

## Architecture references

- `docs/developers/architecture/services/agent/agent-forwarder-architecture.md` — agent forwarder pipeline (traffic events live there, not here).
- `docs/developers/architecture/cross-cutting/observability/audit-pipeline-architecture.md` — agent's local audit + upload path.
