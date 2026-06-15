import { describe, it, expect } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Routes, Route } from 'react-router-dom';

import { renderWithRouter, server, http, HttpResponse } from '@/test/test-utils';
import { LogsTab } from '../../../../../../src/pages/infrastructure/_shared/tabs/logs/LogsTab';
import { MetricsTab } from '../../../../../../src/pages/infrastructure/_shared/tabs/metrics/MetricsTab';
import InfraNodeDetailPage from '../../../../../../src/pages/infrastructure/nodes/InfraNodeDetailPage';

const sampleError = {
  id: 'evt-error-1',
  nodeId: 'thing-abc',
  nodeType: 'agent',
  occurredAt: '2026-04-27T10:00:00Z',
  receivedAt: '2026-04-27T10:00:01Z',
  level: 'error',
  eventType: 'agent.error',
  source: 'relay',
  message: 'dial to upstream failed',
  messageHash: 'a8f23deadbeef',
  attrs: { url: 'https://api.openai.com', err: 'EOF' },
  repeatCount: 1,
  agentVersion: '1.4.2',
};

const sampleFatal = {
  id: 'evt-fatal-1',
  nodeId: 'thing-abc',
  nodeType: 'agent',
  occurredAt: '2026-04-27T09:30:00Z',
  receivedAt: '2026-04-27T09:30:01Z',
  level: 'fatal',
  eventType: 'agent.crash',
  source: 'main',
  message: 'panic: nil deref',
  messageHash: 'b1c4cafe1234',
  stackTrace: 'goroutine 1 [running]:\nmain.run()\n\t/x.go:42',
  repeatCount: 1,
  agentVersion: '1.4.2',
};

function renderLogs(thingId = 'thing-abc') {
  return renderWithRouter(
    <Routes>
      <Route path="/l" element={<LogsTab thingId={thingId} />} />
    </Routes>,
    { route: '/l' },
  );
}

describe('LogsTab', () => {
  it('TestLogsTab_RendersEventsForThing scopes the list query to the urlNodeId', async () => {
    const seenNodeIds: string[] = [];
    server.use(
      http.get('/api/admin/diag-events', ({ request }) => {
        const url = new URL(request.url);
        const nodeId = url.searchParams.get('nodeId') ?? '';
        seenNodeIds.push(nodeId);
        const level = url.searchParams.get('level');
        if (level === 'fatal') return HttpResponse.json({ data: [sampleFatal], nextCursor: '' });
        if (level === 'error') return HttpResponse.json({ data: [sampleError], nextCursor: '' });
        return HttpResponse.json({ data: [], nextCursor: '' });
      }),
    );

    renderLogs('thing-abc');

    await waitFor(() => {
      expect(screen.getByText(/dial to upstream failed/i)).toBeInTheDocument();
    });
    expect(screen.getByText(/panic: nil deref/i)).toBeInTheDocument();
    expect(seenNodeIds.every((id) => id === 'thing-abc')).toBe(true);
  });

  it('TestLogsTab_FiltersByLevel issues parallel error + fatal calls when combined is selected', async () => {
    const seenLevels: string[] = [];
    server.use(
      http.get('/api/admin/diag-events', ({ request }) => {
        const url = new URL(request.url);
        const level = url.searchParams.get('level') ?? '';
        seenLevels.push(level);
        return HttpResponse.json({ data: [], nextCursor: '' });
      }),
    );

    renderLogs();

    await waitFor(() => {
      expect(seenLevels).toContain('error');
      expect(seenLevels).toContain('fatal');
    });
    expect(seenLevels.every((level) => level === 'error' || level === 'fatal')).toBe(true);
  });

  it('TestLogsTab_OpensDetailPanel on row click', async () => {
    server.use(
      http.get('/api/admin/diag-events', ({ request }) => {
        const url = new URL(request.url);
        const level = url.searchParams.get('level');
        if (level === 'error') return HttpResponse.json({ data: [sampleError], nextCursor: '' });
        if (level === 'fatal') return HttpResponse.json({ data: [sampleFatal], nextCursor: '' });
        return HttpResponse.json({ data: [], nextCursor: '' });
      }),
    );

    const user = userEvent.setup();
    renderLogs();

    await waitFor(() => {
      expect(screen.getByText(/dial to upstream failed/i)).toBeInTheDocument();
    });

    await user.click(screen.getAllByText(/dial to upstream failed/i)[0]);

    await waitFor(() => {
      expect(screen.getAllByText(/relay/i).length).toBeGreaterThan(0);
    });
    await waitFor(() => {
      expect(screen.getByText(/EOF/)).toBeInTheDocument();
    });
  });

  it('TestLogsTab_LoadMoreUsesCursor advances the cursor on the next call', async () => {
    let firstCall = true;
    const seenCursors: string[] = [];
    server.use(
      http.get('/api/admin/diag-events', ({ request }) => {
        const url = new URL(request.url);
        const cursor = url.searchParams.get('cursor');
        const level = url.searchParams.get('level');
        seenCursors.push(`${level}:${cursor ?? ''}`);

        if (firstCall) {
          if (level === 'error') return HttpResponse.json({ data: [sampleError], nextCursor: 'err-c1' });
          if (level === 'fatal') return HttpResponse.json({ data: [sampleFatal], nextCursor: 'fat-c1' });
        }

        return HttpResponse.json({ data: [], nextCursor: '' });
      }),
    );

    const user = userEvent.setup();
    renderLogs();

    await waitFor(() => {
      expect(screen.getByText(/dial to upstream failed/i)).toBeInTheDocument();
    });

    firstCall = false;
    const loadMore = await screen.findByRole('button', { name: /load more/i });
    await user.click(loadMore);

    await waitFor(() => {
      expect(seenCursors.some((cursor) => cursor === 'error:err-c1')).toBe(true);
      expect(seenCursors.some((cursor) => cursor === 'fatal:fat-c1')).toBe(true);
    });
  });
});

describe('Cross-tab time-axis sync', () => {
  it('TestNodeDetail_TimeAxisSharedAcrossTabs propagates from / to URL params', async () => {
    const diagFromTo: Array<{ from: string; to: string }> = [];
    server.use(
      http.get('/api/admin/diag-events', ({ request }) => {
        const url = new URL(request.url);
        diagFromTo.push({
          from: url.searchParams.get('from') ?? '',
          to: url.searchParams.get('to') ?? '',
        });
        return HttpResponse.json({ data: [], nextCursor: '' });
      }),
    );

    const user = userEvent.setup();
    renderWithRouter(
      <Routes>
        <Route path="/infrastructure/nodes" element={<div>list</div>} />
        <Route path="/infrastructure/nodes/:id" element={<InfraNodeDetailPage />} />
      </Routes>,
      { route: '/infrastructure/nodes/node-gw-1' },
    );

    await waitFor(() => {
      expect(screen.getByRole('tab', { name: /^metrics$/i })).toBeInTheDocument();
    });
    await user.click(screen.getByRole('tab', { name: /^metrics$/i }));

    const sevenDayBtn = await screen.findByRole('button', { name: /^7d$/i });
    await user.click(sevenDayBtn);

    await user.click(screen.getByRole('tab', { name: /^diagnostics$/i }));

    await waitFor(() => {
      expect(diagFromTo.length).toBeGreaterThan(0);
    });

    const last = diagFromTo[diagFromTo.length - 1];
    const span = new Date(last.to).getTime() - new Date(last.from).getTime();
    const sevenDays = 7 * 24 * 60 * 60 * 1000;
    expect(Math.abs(span - sevenDays)).toBeLessThan(60_000);
  });

  it('TestMetricsTabAndLogsTab_DefaultRangeAlignsToOneHour when URL has no params', async () => {
    const seenSpans: number[] = [];
    server.use(
      http.get('/api/admin/diag-events', ({ request }) => {
        const url = new URL(request.url);
        const from = url.searchParams.get('from') ?? '';
        const to = url.searchParams.get('to') ?? '';
        if (from && to) seenSpans.push(new Date(to).getTime() - new Date(from).getTime());
        return HttpResponse.json({ data: [], nextCursor: '' });
      }),
      http.get('/api/admin/ops-metrics/timeseries', ({ request }) => {
        const url = new URL(request.url);
        const from = url.searchParams.get('from') ?? '';
        const to = url.searchParams.get('to') ?? '';
        if (from && to) seenSpans.push(new Date(to).getTime() - new Date(from).getTime());
        return HttpResponse.json({ data: [], granularity: 'raw' });
      }),
    );

    renderWithRouter(
      <Routes>
        <Route path="/m" element={<MetricsTab thingId="t1" thingType="agent" />} />
        <Route path="/l" element={<LogsTab thingId="t1" />} />
      </Routes>,
      { route: '/m' },
    );

    await waitFor(() => {
      expect(seenSpans.length).toBeGreaterThan(0);
    });
    expect(seenSpans.every((span) => Math.abs(span - 60 * 60 * 1000) < 5_000)).toBe(true);
  });
});
