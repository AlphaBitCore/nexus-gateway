/**
 * TrafficEventDetail tests. Verifies:
 *   - waterfall mounts with a non-empty phase mix
 *   - waterfall "no data" copy when every phase is 0
 *   - hook list renders rows from JSON-encoded payload
 *   - hook list handles unparseable payload gracefully
 *   - null phase fields are tolerated (intercept_ms-only flows)
 *   - close button fires onClose
 */
import type { ReactElement } from 'react';
import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import {
  DEFAULT_THEME,
  ThemeContext,
  type ThemeContextValue,
} from '@nexus-gateway/ui-shared';

import { TrafficEventDetail } from './TrafficEventDetail';
import type { AgentEvent } from '@/api/agent';

// Minimum ThemeContext stub: TrafficEventDetail's Waterfall sub-
// component reads `resolvedMode` only, but the context shape is the
// full ThemeContextValue so we satisfy every field with DEFAULT_THEME
// + no-op setters. Wrapping in the real <ThemeProvider> would trigger
// loadTheme() / matchMedia / localStorage side-effects that fail under
// jsdom; the direct Context.Provider avoids all of them.
const themeStub: ThemeContextValue = {
  mode: 'light',
  resolvedMode: 'light',
  setMode: () => {},
  theme: DEFAULT_THEME,
  themeId: 'default',
  setThemeId: () => {},
  brand: DEFAULT_THEME.brand,
};

function renderWithTheme(ui: ReactElement) {
  return render(
    <ThemeContext.Provider value={themeStub}>{ui}</ThemeContext.Provider>,
  );
}

function sampleEvent(overrides: Partial<AgentEvent> = {}): AgentEvent {
  return {
    id: 'evt-abc',
    timestamp: '2026-05-14T12:00:00Z',
    sourceProcess: 'curl',
    targetHost: 'api.openai.com',
    statusCode: 200,
    latencyMs: 1200,
    requestHooksMs: 5,
    responseHooksMs: 8,
    upstreamTtfbMs: 700,
    upstreamTotalMs: 1150,
    hooksPipeline: null,
    ...overrides,
  } as AgentEvent;
}

describe('TrafficEventDetail', () => {
  it('renders null when no event is supplied', () => {
    const { container } = renderWithTheme(
      <TrafficEventDetail event={null} onClose={() => {}} />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it('mounts the waterfall with measurable phases', () => {
    renderWithTheme(<TrafficEventDetail event={sampleEvent()} onClose={() => {}} />);

    expect(screen.getByText(/Phase Breakdown/i)).toBeInTheDocument();
    // legend entries — case-insensitive, ignore order
    expect(screen.getByText(/Upstream TTFB/i)).toBeInTheDocument();
    expect(screen.getByText(/Upstream Body/i)).toBeInTheDocument();
    expect(screen.getByText(/Our Overhead/i)).toBeInTheDocument();
    // header dl renders the event id
    expect(screen.getByText('evt-abc')).toBeInTheDocument();
  });

  it('shows "no data" when every phase is zero', () => {
    renderWithTheme(
      <TrafficEventDetail
        event={sampleEvent({
          latencyMs: 0,
          requestHooksMs: 0,
          responseHooksMs: 0,
          upstreamTtfbMs: 0,
          upstreamTotalMs: 0,
        })}
        onClose={() => {}}
      />,
    );

    expect(screen.getByText(/No phase data available/i)).toBeInTheDocument();
  });

  it('renders hook rows from the JSON payload', () => {
    renderWithTheme(
      <TrafficEventDetail
        event={sampleEvent({
          hooksPipeline: JSON.stringify([
            { hookId: 'pii-scanner', stage: 'request', decision: 'redact', latencyMs: 4 },
            { hookId: 'rate-limit', stage: 'request', decision: 'allow', latencyMs: 1 },
          ]),
        })}
        onClose={() => {}}
      />,
    );

    expect(screen.getByText('pii-scanner')).toBeInTheDocument();
    expect(screen.getByText('rate-limit')).toBeInTheDocument();
  });

  it('shows the parse-error fallback when hooksPipeline is malformed', () => {
    renderWithTheme(
      <TrafficEventDetail
        event={sampleEvent({ hooksPipeline: '{not json' })}
        onClose={() => {}}
      />,
    );

    expect(screen.getByText(/unparseable/i)).toBeInTheDocument();
  });

  it('tolerates null phase fields (intercept_ms-only flows)', () => {
    renderWithTheme(
      <TrafficEventDetail
        event={sampleEvent({
          latencyMs: 100,
          upstreamTtfbMs: null,
          upstreamTotalMs: null,
          requestHooksMs: null,
          responseHooksMs: null,
        })}
        onClose={() => {}}
      />,
    );

    // No throw, dialog renders, phase-breakdown header still present.
    expect(screen.getByRole('dialog')).toBeInTheDocument();
    expect(screen.getByText(/Phase Breakdown/i)).toBeInTheDocument();
  });

  it('fires onClose when the close button is clicked', () => {
    const onClose = vi.fn();
    renderWithTheme(<TrafficEventDetail event={sampleEvent()} onClose={onClose} />);

    fireEvent.click(screen.getByLabelText(/Close/i));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  // Waterfall must fall back to upstreamTtfbMs when upstreamTotalMs is null
  // so "Our Overhead" doesn't silently absorb the upstream wait.
  it('falls back to upstreamTtfbMs when upstreamTotalMs is null', () => {
    renderWithTheme(
      <TrafficEventDetail
        event={sampleEvent({
          latencyMs: 7597,
          upstreamTtfbMs: 1636,
          upstreamTotalMs: null,
          requestHooksMs: null,
          responseHooksMs: null,
        })}
        onClose={() => {}}
      />,
    );

    // Without the fallback, "Our Overhead" would render as 7597ms
    // (the entire latency). With the fallback, it renders as
    // 5961ms (= 7597 - 1636).
    expect(screen.getByText(/5\.96s|5961ms/)).toBeInTheDocument();
    // TTFB segment still renders independently.
    expect(screen.getByText(/1\.64s|1636ms/)).toBeInTheDocument();
  });
});
