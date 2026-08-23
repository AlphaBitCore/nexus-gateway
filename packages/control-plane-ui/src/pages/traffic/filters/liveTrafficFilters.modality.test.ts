import { describe, it, expect } from 'vitest';
import {
  buildTrafficAuditLogQueryParams,
  describeLiveTrafficFilters,
  EMPTY_LIVE_TRAFFIC_FILTERS,
} from './liveTrafficFilters';

const PAGE = { limit: 20, offset: 0 };

function endpointTypeParam(modality: string): string | null {
  const params = buildTrafficAuditLogQueryParams(
    { ...EMPTY_LIVE_TRAFFIC_FILTERS, modality },
    PAGE,
  );
  return params.get('endpointType');
}

describe('Modality filter → endpointType query param', () => {
  it('maps a single-kind modality to that endpoint kind', () => {
    // image / tts / stt / etc. map 1:1 to their endpoint_type.
    expect(endpointTypeParam('image_generation')).toBe('image_generation');
    expect(endpointTypeParam('tts')).toBe('tts');
    expect(endpointTypeParam('rerank')).toBe('rerank');
  });

  it('expands the chat modality to BOTH chat and responses', () => {
    // The one modality that spans two endpoint kinds — /v1/chat/completions
    // and /v1/responses are the same text conversation. Filtering "Chat" must
    // not silently drop responses rows, which is why the store accepts a list.
    expect(endpointTypeParam('chat')).toBe('chat,responses');
  });

  it('sends no endpointType param when no modality is selected', () => {
    expect(endpointTypeParam('')).toBeNull();
  });

  it('ignores an unknown modality (no kinds → no filter) rather than sending an empty predicate', () => {
    // A stale/unknown value must not produce endpointType='' which the backend
    // would treat as "no rows" — it must simply apply no modality filter.
    expect(endpointTypeParam('not_a_modality')).toBeNull();
  });

  it('surfaces the modality in the active-filter description so it counts and is clearable', () => {
    const lines = describeLiveTrafficFilters({ ...EMPTY_LIVE_TRAFFIC_FILTERS, modality: 'image_generation' });
    expect(lines.some((l) => l.startsWith('Modality:'))).toBe(true);
  });
});
