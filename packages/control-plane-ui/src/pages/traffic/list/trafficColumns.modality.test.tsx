import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';
import { getColumnsForSource, modalityOfEndpointKind } from './trafficColumns';
import { formatUsdSci } from '@/lib/format';
import type { TrafficEvent } from '../../../api/types';
import en from '@/i18n/locales/en/pages.json';
import es from '@/i18n/locales/es/pages.json';
import zh from '@/i18n/locales/zh/pages.json';

// The Modality column reports a MODALITY derived from traffic_event.endpoint_type. Two
// defects were reported against it from the live Traffic page, and they had different
// causes even though they looked like one:
//
// 1. Chat rows rendered a dash. It was deliberate ("keep the column quiet for the common
//    case") and still wrong: on an audit surface a blank cell is indistinguishable from
//    missing data, so a reader has to already know the convention to interpret it.
// 2. `responses` rendered as a modality of its own. endpoint_type is an ENDPOINT KIND, not
//    a modality — /v1/responses is the same text conversation as /v1/chat/completions
//    through a different request shape. The column showed an ingress distinction for
//    exactly one of the four text ingresses, arbitrarily, because the other three happen to
//    stamp endpoint_type=chat.
//
// Both are guarded below, plus the class each belongs to.

// Mirrors typology.EndpointKind (packages/shared/transport/typology/endpointkind.go), whose
// exhaustiveness is asserted Go-side against AllEndpointKinds. A kind reaching the UI with
// no mapping renders nothing at all, which is why the mapping must be total.
const ENDPOINT_KINDS = [
  'chat',
  'responses',
  'embeddings',
  'rerank',
  'image_generation',
  'tts',
  'stt',
  'video_generation',
  'batch',
  'job',
  'models',
  'guardrail',
  'realtime',
];

// Minimal identity translator: resolves a key to its EN leaf so assertions read as an
// operator sees the cell.
const t = (key: string, fallback?: any) => {
  const path = key.replace(/^pages:/, '').split('.');
  let node: any = en;
  for (const p of path) {
    node = node?.[p];
    if (node === undefined) return typeof fallback === 'string' ? fallback : key;
  }
  return typeof node === 'string' ? node : key;
};

function renderModality(endpointType: string | null | undefined) {
  const col = getColumnsForSource('vk', t).find((c: any) => c.key === 'endpointType');
  if (!col) throw new Error('the vk traffic table has no modality column');
  const { container } = render(<>{(col as any).render({ endpointType } as TrafficEvent)}</>);
  return container.textContent ?? '';
}

describe('traffic Modality column', () => {
  it('labels a chat row instead of leaving the cell blank', () => {
    // The reported defect: nine rows of ordinary chat traffic all showed "-", so the most
    // common case in the product read as "no modality recorded".
    expect(renderModality('chat')).toBe(en.traffic.modality.chat);
  });

  it('reads a /v1/responses row as chat, because it is the same modality', () => {
    // The second reported defect. Both must resolve to the SAME cell text — asserting the
    // literal alone would still pass if someone gave responses its own label that happened
    // to say "Chat", and the mapping would be free to drift apart again.
    expect(modalityOfEndpointKind('responses')).toBe(modalityOfEndpointKind('chat'));
    expect(renderModality('responses')).toBe(renderModality('chat'));
  });

  it('keeps genuinely different modalities distinct', () => {
    // Guards the over-correction: collapsing responses into chat must not be implemented as
    // "everything is chat".
    const distinct = ['chat', 'image_generation', 'tts', 'stt', 'video_generation', 'embeddings'];
    const rendered = distinct.map(renderModality);
    expect(new Set(rendered).size).toBe(distinct.length);
  });

  it('shows a dash only when the row genuinely carries no endpoint_type', () => {
    // A dash must mean exactly one thing. Without this, "always render something" could be
    // satisfied by printing a placeholder for missing data too, and the column would be
    // ambiguous again in the other direction.
    expect(renderModality(null)).toBe('-');
    expect(renderModality(undefined)).toBe('-');
    expect(renderModality('')).toBe('-');
  });

  it('maps every EndpointKind to a modality that all three locales can label', () => {
    // The class fix for defect 2. A per-value patch would leave the next kind to leak, which
    // is exactly how `responses` survived: the render predicate was open-ended ("anything
    // not chat") while the label table listed eight specific multimodal kinds.
    const unmapped = ENDPOINT_KINDS.filter((k) => !modalityOfEndpointKind(k));
    expect(unmapped, `EndpointKinds with no modality mapping: ${unmapped.join(', ')}`).toEqual([]);

    const modalities = [...new Set(ENDPOINT_KINDS.map((k) => modalityOfEndpointKind(k)!))];
    for (const locale of [
      { name: 'en', bundle: en },
      { name: 'es', bundle: es },
      { name: 'zh', bundle: zh },
    ]) {
      const map = (locale.bundle as any).traffic.modality as Record<string, string>;
      const missing = modalities.filter((m) => !map[m]);
      expect(missing, `${locale.name} has no Modality label for: ${missing.join(', ')}`).toEqual([]);
      const orphan = Object.keys(map).filter((m) => !modalities.includes(m));
      expect(orphan, `${locale.name} labels modalities nothing maps to: ${orphan.join(', ')}`).toEqual([]);
    }
  });
});

// The Cost column recomputes from tokens × the price snapshot so it tracks
// current catalog prices. Modality rows (image / tts / rerank) carry no
// prompt/completion tokens, so that recompute is 0 and the cell used to show a
// dash even though the gateway stamped a real per-unit cost. It now falls back
// to estimated_cost_usd for exactly those rows.
function renderCost(row: Partial<TrafficEvent>): string {
  const col = getColumnsForSource('vk', t).find((c: any) => c.key === 'upstreamCostUsd');
  if (!col) throw new Error('the vk traffic table has no cost column');
  const { container } = render(<>{(col as any).render(row as TrafficEvent)}</>);
  return container.textContent ?? '';
}

describe('traffic Cost column — modality fallback', () => {
  it('shows the stamped per-unit cost for a rerank row that has no tokens', () => {
    // A Cohere rerank row: priced per search unit, zero prompt/completion
    // tokens, so the token recompute is 0. The cell must show the stamped
    // $0.002, not a dash.
    const cell = renderCost({
      endpointType: 'rerank',
      promptTokens: 0,
      completionTokens: 0,
      estimatedCostUsd: 0.002,
    });
    expect(cell).toBe(formatUsdSci(0.002));
    expect(cell).not.toBe('-');
  });

  it('still shows a dash when there are no tokens AND no stamped cost', () => {
    // The dash must remain meaningful: genuinely unpriced / cost-less rows.
    expect(renderCost({ endpointType: 'rerank', promptTokens: 0, estimatedCostUsd: 0 })).toBe('-');
    expect(renderCost({ endpointType: 'rerank', promptTokens: 0 })).toBe('-');
  });

  it('prefers the token recompute over the stamped cost when tokens exist', () => {
    // A chat row with tokens must keep tracking current catalog prices, not the
    // historical stamp. 1000 prompt × $5/M + 500 completion × $15/M = $0.0125;
    // the bogus stamped value below must be ignored.
    const cell = renderCost({
      endpointType: 'chat',
      promptTokens: 1000,
      completionTokens: 500,
      modelInputPricePerMillion: 5,
      modelOutputPricePerMillion: 15,
      estimatedCostUsd: 999,
    });
    expect(cell).toBe(formatUsdSci(0.0125));
    expect(cell).not.toBe(formatUsdSci(999));
  });
});
