# P3 per-role effective permission diff

Computed from `tools/db-migrate/seed/seed.ts` block 13 (deleted in P3) + block 15c (rewritten).
Effective set excludes phantom actions (not present in `allAdminActions`) and non-admin namespaces.

## compliance-team

- Policies attached: `NexusComplianceAdmin`, `NexusSecurityAdminAccess`
- Old effective action count: **55**
- New effective action count: **47**
- **Diff: 0 gained, 8 lost.**

Lost (8) — these permissions DISAPPEAR after P3:
- `admin:kill-switch.toggle`  ⚠️
- `admin:node.write-override`  ⚠️
- `admin:routing-rule.create`  ⚠️
- `admin:routing-rule.delete`  ⚠️
- `admin:routing-rule.update`  ⚠️
- `admin:virtual-key.create`  ⚠️
- `admin:virtual-key.delete`  ⚠️
- `admin:virtual-key.update`  ⚠️

## developers

- Policies attached: `NexusGatewayInvokeAll`
- Old effective action count: **0**
- New effective action count: **0**
- **Diff: 0 actions gained, 0 actions lost.** ✓

## members

- Policies attached: `NexusAgentAccess`
- Old effective action count: **0**
- New effective action count: **0**
- **Diff: 0 actions gained, 0 actions lost.** ✓

## provider-admins

- Policies attached: `NexusProviderAdminAccess`
- Old effective action count: **65**
- New effective action count: **65**
- **Diff: 0 actions gained, 0 actions lost.** ✓

## provider-managers

- Policies attached: `NexusProviderAdmin`, `NexusProviderAdminAccess`
- Old effective action count: **65**
- New effective action count: **65**
- **Diff: 0 actions gained, 0 actions lost.** ✓

## security-admins

- Policies attached: `NexusSecurityAdminAccess`
- Old effective action count: **47**
- New effective action count: **47**
- **Diff: 0 actions gained, 0 actions lost.** ✓

## super-admins

- Policies attached: `NexusSuperAdmin`, `NexusAdminFullAccess`
- Old effective action count: **95**
- New effective action count: **103**
- **Diff: 8 gained, 0 lost.**

Gained (8):
- `admin:compliance-report.read`
- `admin:diagnostic-mode.read`
- `admin:diagnostic-mode.update`
- `admin:dsar.read`
- `admin:interception-domain.read`
- `admin:interception-domain.update`
- `admin:model-pricing.read`
- `admin:node.read`

## viewers

- Policies attached: `NexusViewer`, `NexusViewerAccess`
- Old effective action count: **29**
- New effective action count: **29**
- **Diff: 0 actions gained, 0 actions lost.** ✓

