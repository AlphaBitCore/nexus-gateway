/**
 * RoutingWalk — the dispatch walk as an operator reads it.
 *
 * The walk's invariants are all written to traffic_event.routing_trace by the
 * gateway: the selection reason per attempt, the error class, whether a target
 * was dispatched to or passed over. They were true and unreadable — the drawer
 * handed the whole column over as raw JSON, so "an operator can replay this
 * walk" meant reading JSON or writing SQL.
 *
 * These assert what the rendered surface actually SAYS, not that a component
 * mounted.
 */
import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import { RoutingWalk, walkAttempts } from '@/pages/traffic/audit-drawer/RoutingWalk';

import enPages from '@/i18n/locales/en/pages.json';
import zhPages from '@/i18n/locales/zh/pages.json';
import esPages from '@/i18n/locales/es/pages.json';

// A walk that took a rate limit on one provider, stepped to another, and was
// then stopped short — the shape the whole recovery engine exists to produce.
const trace = {
  attempts: [
    {
      seq: 1,
      provider: 'openai',
      model: 'gpt-5-mini',
      dispatched: true,
      status: 429,
      code: 'rate_limited',
      errorClass: 'rate_limited',
      selectionReason: 'next-in-list',
      latencyMs: 812,
      coerced: ['max_tokens→max_completion_tokens'],
      error: 'slow down',
    },
    {
      seq: 2,
      provider: 'anthropic',
      model: 'claude-haiku',
      dispatched: true,
      status: 500,
      code: 'upstream_error',
      errorClass: 'upstream_error',
      selectionReason: 'different-provider',
      latencyMs: 1204,
    },
    {
      seq: 3,
      provider: 'gemini',
      model: 'gemini-flash',
      dispatched: false,
      error: 'skipped: upstream call budget exhausted',
    },
  ],
};

function renderWalk(t: unknown) {
  return render(
    <RoutingWalk
      trace={t}
      tTitle="Dispatch Walk"
      tDispatched="dispatched"
      tSkipped="skipped"
      tCoerced="rewrote"
    />,
  );
}

describe('RoutingWalk', () => {
  it('names every target the walk touched, in order', () => {
    renderWalk(trace);
    expect(screen.getByText('openai')).toBeInTheDocument();
    expect(screen.getByText('anthropic')).toBeInTheDocument();
    // The target that never ran is the one an operator is most often trying to
    // account for; dropping it would make the chain look complete when it is
    // one entry short.
    expect(screen.getByText('gemini')).toBeInTheDocument();
    expect(screen.getByText('gemini-flash')).toBeInTheDocument();
  });

  it('separates a call that reached an upstream from a target passed over', () => {
    renderWalk(trace);
    expect(screen.getAllByText('dispatched')).toHaveLength(2);
    expect(screen.getAllByText('skipped')).toHaveLength(1);
  });

  it('says WHY the walk went where it did', () => {
    renderWalk(trace);
    // Selection is not positional. Without the reason on the row, a chain that
    // steps from openai to anthropic is indistinguishable from a bug.
    expect(screen.getByText('different-provider')).toBeInTheDocument();
    expect(screen.getByText('next-in-list')).toBeInTheDocument();
  });

  it('shows the status and the error text that explain each failure', () => {
    renderWalk(trace);
    expect(screen.getByText('429')).toBeInTheDocument();
    expect(screen.getByText('500')).toBeInTheDocument();
    expect(screen.getByText('slow down')).toBeInTheDocument();
    expect(screen.getByText('skipped: upstream call budget exhausted')).toBeInTheDocument();
  });

  it('does not print the class twice when it is the same word as the code', () => {
    renderWalk(trace);
    // code and errorClass are one word wherever they describe one failure.
    expect(screen.getAllByText('rate_limited')).toHaveLength(1);
  });

  it('names the fields we rewrote before the dispatch that failed', () => {
    renderWalk(trace);
    // The response header carrying this reaches a caller who discards it. An
    // operator asking "what did we change" hours later has this row and
    // nothing else — and a coercion that PRECEDED a failure is the one they
    // most want, because "we rewrote this and then it 400ed" is the question.
    expect(screen.getByText(/max_tokens→max_completion_tokens/)).toBeInTheDocument();
  });

  it('says nothing about coercion on an attempt that had none', () => {
    renderWalk(trace);
    // Exactly one row was coerced; a label on the other two would read as a
    // rewrite that did not happen.
    expect(screen.getAllByText(/rewrote/)).toHaveLength(1);
  });

  it('renders nothing for a row whose trace predates the walk', () => {
    // Rows written by three data planes across several schema versions land in
    // this column. Anything that is not a walk must disappear rather than throw.
    for (const t of [undefined, null, {}, { attempts: 'nope' }, { attempts: [] }, 'string', 42]) {
      const { container } = renderWalk(t);
      expect(container).toBeEmptyDOMElement();
    }
  });

  it('survives entries missing every optional field', () => {
    const { container } = renderWalk({ attempts: [{}, { seq: 9 }] });
    expect(container).not.toBeEmptyDOMElement();
    expect(screen.getByText('9')).toBeInTheDocument();
  });

  it('numbers entries by position when seq is absent', () => {
    expect(walkAttempts({ attempts: [{}, {}] }).map((a) => a.seq)).toEqual([1, 2]);
  });

  it('has its labels in every locale', () => {
    for (const pages of [enPages, zhPages, esPages]) {
      const routing = (pages as { traffic: { detail: { routing: Record<string, string> } } }).traffic
        .detail.routing;
      for (const key of ['walkTitle', 'walkDispatched', 'walkSkipped', 'walkCoerced']) {
        expect(routing[key], `${key} missing`).toBeTruthy();
      }
    }
  });
});
