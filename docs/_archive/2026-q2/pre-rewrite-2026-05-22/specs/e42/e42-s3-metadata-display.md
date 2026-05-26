# E42-S3 — Node Detail Metadata Display (SDD)

## User story

As an operator, when I open the Overview tab of a Node detail page I can
see the contents of `thing.metadata` — the JSONB labels and host
information written by `selfreg` and by the agent enrollment flow — so I
can answer questions like "which user enrolled this device?", "what OS
and hostname is this Hub running on?", and "which role is this CP
instance playing?" without dropping into psql.

## Tasks

1. **API audit.** Open
   `packages/control-plane/internal/handler/admin_things*.go` and confirm
   the `GET /api/admin/nodes/:id` (and its list cousin) response includes
   the `metadata` field projected from `thing.metadata`. The shape used
   today by the Configuration tab pulls metadata via a different
   endpoint; for the Overview tab we need the field on the node detail
   payload. If absent, add a `Metadata json.RawMessage \`json:"metadata,omitempty"\``
   field to the response struct and project from `thing.metadata`.
2. **Frontend types.** Update
   `packages/control-plane-ui/src/api/types.ts` (or the dedicated nodes
   types file if one exists) so the `InfraNode` type carries
   `metadata?: Record<string, unknown> | null`.
3. **UI component.**
   - Add a new component `MetadataPanel` in
     `packages/control-plane-ui/src/pages/infrastructure/MetadataPanel.tsx`.
   - Two zones: a "Common" grid that surfaces a curated list of keys with
     friendly labels (`hostname`, `os`, `osVersion`, `enrolledBy`, `role`,
     `metricsUrl`, `schedulerEnabled`, `source_ip`, `pid`, `auth_type`,
     `conn_protocol`); each key shown only when present in the payload.
     A "Custom" grid below for any remaining keys not in the curated list.
   - Below both grids, a `<details>`-style collapsible "Raw JSON" block
     that pretty-prints the full metadata object. Default collapsed.
   - Empty state (metadata absent or `{}`): render a single muted line
     `t('overview.metadata.empty')`.
4. **Page integration.**
   - Mount `MetadataPanel` under the existing InfoRow grid on the
     Overview tab of `InfraNodeDetailPage.tsx`.
   - Pass `metadata={node.metadata ?? null}`.
5. **i18n.** Add the following keys to
   `packages/control-plane-ui/src/i18n/locales/{en,zh,es}/pages.json`
   under the `infrastructure.overview.metadata` namespace, then mirror
   into `public/locales/{en,zh,es}/pages.json`:
   - `title` — "Metadata" / "Metadatos" (zh in locale file)
   - `empty` — "No metadata recorded for this node."
   - `commonLabel.*` — friendly labels for the curated keys.
   - `customSectionLabel` — "Custom labels"
   - `rawSummary` — "Raw JSON"
   - `expandRaw` — "Show JSON"
   - `collapseRaw` — "Hide JSON"
   Technical tokens (`auth_type`, `conn_protocol`, key names like
   `pid`, `os`) stay literal across all locales per repo policy.

## Out of scope

- Editing metadata from the UI. `thing.metadata` is written by services
  (selfreg / enrollment); the Overview tab is read-only.
- Adding metadata to other surfaces (Nodes list table, Sync page). The
  detail view is the natural home.
- Validating metadata schema. JSONB stays flexible per thing-model
  Section 9.

## Acceptance criteria

- [ ] Visiting `/infrastructure/nodes/hub-dev`, the Overview tab shows a
      Metadata section containing the keys written by `selfreg`:
      `hostname`, `pid`, `role`, `metricsUrl`, `schedulerEnabled`.
- [ ] Visiting an agent node, the Metadata section shows the agent-side
      labels (`os`, `osVersion`, `hostname`, `enrolledBy`).
- [ ] If `thing.metadata` is `null` or `{}`, the empty-state copy renders
      and the JSON details element is not shown.
- [ ] The "Show JSON" button toggles the raw JSON code block; default
      state is collapsed.
- [ ] All new copy renders correctly in `en`, `zh`, and `es`
      (`i18n-gap-check` reports no missing keys for the new namespace).
