# Agent UI — Traffic (intercepted connections)

Traffic is a timeline of the outbound network connections this agent intercepted, written for the device's own user rather than a fleet admin — it stays at the level of "what app, where, and was it allowed?". The engineering signals (ports, bytes, IP, the hook pipeline) live in a click-to-open detail drawer. It is distinct from the **Activity** page, which lists the agent's own lifecycle events. The page is `packages/agent/ui/frontend/src/pages/traffic/Traffic.tsx`.

## The list

Each row shows the time (relative, with the absolute time on hover), the originating app (process name), the site (hostname), the HTTP method, the path, the latency (human-readable), a status, and tags.

The **status** reflects what the agent did with the connection: `Inspected` (green), `Processed` (amber), `Blocked` (red), `Bump failed` (red), or `Untracked` (muted). The **tags** column adds an `AI` chip when the destination matched an interception domain, a `hook · <decision>` chip when a hook actually ran (red for a deny / reject / soft-block decision, amber for approve), and a `policy` chip when a policy rule applied.

## Filters and controls

- **Action filter** — All, AI, Blocked, or Processed.
- **Time window** — 1 hour, 24 hours, 7 days, 30 days, or all; defaults to 24 hours. The window is pushed down into the daemon's query rather than filtered in the browser.
- **Search** — a server-side search over the events, debounced as you type.
- **Page size** — 10, 25, 50, or 100 rows (default 10).
- **Auto-refresh** — off, 5s, 15s, 30s, 1m, or 5m (default 5s); the choice is remembered across restarts.

## Detail drawer

Clicking a row opens a drawer with the engineering detail for that connection:

- A **latency waterfall** breaking the time into request hooks, the agent's own overhead, upstream time-to-first-byte, upstream body, and response hooks.
- The **event fields**: time, event id, process, target, action, hook decision, latency, status, method, path or URL, destination IP and port, and bytes in and out.
- The **hook pipeline** — the hooks that executed for this event (with empty and unparseable states handled).
- The **payload viewer** — request and response bodies with a Raw / Normalized tab switch (Normalized is disabled when no normalized projection was captured — non-inspected flows). Each captured direction renders as a typed, readable projection with a provenance badge: **Tier 1** (exact protocol decoder, with its confidence score), **Tier 2** (pattern probe for consumer web surfaces), or the neutral **Structural** badge (no confidence numeral) when no AI protocol was identified and the body is shown as a typed projection of the raw HTTP content — JSON tree, text, form fields, binary digest, or an event-stream frame list (one row per frame with its event-name chip, collapsed beyond the first 50 frames; long streams note the frame view is truncated while the full stream stays in the Raw view). Chat rows show role bubbles, and the usage row includes reasoning tokens when the provider reported them. Content a compliance hook redacted is marked inline over the replacement text. When the storage policy kept no readable copy at all, the view shows a notice instead of the content, and the notice distinguishes why: content dropped because the policy says to drop it; or — when the policy was to redact but the redaction could not be safely applied to the stored copy — a separate notice explaining that the copy was dropped as the safe fallback, with the reason in plain words (the machine token shown alongside) and the parts of the payload that could not be resolved. Events recorded before this distinction existed show a neutral "content not stored per the storage policy" notice that does not guess between the two.

- **Media in a payload** — an image, audio clip, video or file attached to a request or returned in a response appears as a card in the normalized view rather than as a wall of base64. Each card states its kind and size, and says plainly where the bytes are:

  | The card says | What it means |
  |---|---|
  | Stored with this request | The bytes are held alongside the event. Images preview in place; everything else downloads as a real file of its own type. |
  | Remote content | The payload referenced a URL. The agent never fetched or stored it, so there is nothing local to open. |
  | Content held by the provider | The payload referenced something by the provider's own id. Only the provider can resolve it. |
  | Bytes not retained | Only a fingerprint was kept — enough to prove which file was sent, not enough to reopen it. |
  | Captured, then expired | The bytes were stored and have since been removed by the retention policy. The fingerprint remains. |
  | Not captured | Capture was off, or the payload declared the media without carrying it. |
  | Capture incomplete | The stored copy is partial or unreadable, so the media cannot be recovered. |

  A card offers a preview or a download **only** when the bytes are actually there. In every other state it says so instead of presenting a control that would fail when clicked.

## Where the data comes from

`agentApi.queryEvents` calls the daemon's events query over the local bridge, reading the agent's own audit-event store — not the Control Plane admin API. The status, AI tag, and filters are all derived from the same client-side classification of each event.

Media bytes never travel a second path: a card reads whatever the stored payload already contains, so what the card shows is exactly what was captured.

## References

- `packages/agent/ui/frontend/src/pages/traffic/Traffic.tsx` — the traffic list, filters, and controls
- `packages/agent/ui/frontend/src/pages/traffic/TrafficEventDetail.tsx` — the detail drawer and latency waterfall
- `packages/agent/ui/frontend/src/components/normalized/NormalizedPayloadView.tsx` — the normalized conversation view and the storage-policy notices
- `packages/ui-shared/src/components/MediaCard/MediaCard.tsx` — the media card shared with the Control Plane traffic drawer, so both surfaces describe custody in the same words
- `packages/agent/ui/frontend/src/lib/classify.ts` — `classify`, `statusDescriptor`, and `isAITraffic`
- `packages/agent/ui/frontend/src/api/agent.ts` — `agentApi.queryEvents` and the `AgentEvent` shape
