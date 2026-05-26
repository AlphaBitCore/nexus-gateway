// Maps a device/Thing status (as returned by the Hub) to a Badge variant.
// The Hub returns lowercase tokens — `online`, `offline`, `enrolled`,
// `out-of-sync`, `revoked`. Legacy uppercase aliases (`ACTIVE`, `OFFLINE`,
// `ENROLLED`, `REVOKED`) are kept for backward compatibility with any
// stored rows still using the older enum. Same shape as the helpers in
// pages/infrastructure/Infra* so colors are consistent across pages.
export function statusVariant(s: string): 'success' | 'warning' | 'default' | 'danger' {
  const map: Record<string, 'success' | 'warning' | 'default' | 'danger'> = {
    online: 'success',
    enrolled: 'warning',
    offline: 'default',
    'out-of-sync': 'danger',
    revoked: 'danger',
    // Uppercase aliases for older AgentDevice rows written before the enum was lowercased.
    ACTIVE: 'success',
    ENROLLED: 'warning',
    OFFLINE: 'default',
    REVOKED: 'danger',
  };
  return map[s] ?? map[s.toLowerCase()] ?? 'default';
}
