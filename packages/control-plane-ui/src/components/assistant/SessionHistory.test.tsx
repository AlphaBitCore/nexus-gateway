import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { SessionHistory } from './SessionHistory';
import type { SessionMeta } from './streamChat';

vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (k: string, o?: Record<string, unknown>) => (o?.filter ? `${k}:${String(o.filter)}` : k) }),
}));

const noop = () => {};

const sessions: SessionMeta[] = [
  { id: 'w1', title: 'cost spike triage', updatedAt: new Date(Date.now() - 5 * 60_000).toISOString() },
  { id: 'd1', title: 'deploy checklist', updatedAt: new Date(Date.now() - 3 * 3_600_000).toISOString() },
  { id: 'o1', title: 'old chat', updatedAt: '' },
];

describe('SessionHistory — filter and relative time', () => {
  it('shows each row’s relative age', () => {
    render(<SessionHistory showHistory sessions={sessions} loadSession={noop} removeSession={noop} />);
    expect(screen.getByText('common:assistant.timeMinAgo')).toBeInTheDocument();
    expect(screen.getByText('common:assistant.timeHourAgo')).toBeInTheDocument();
  });

  it('filters by title as you type and names a miss', () => {
    render(<SessionHistory showHistory sessions={sessions} loadSession={noop} removeSession={noop} />);
    const box = screen.getByLabelText('common:assistant.filterSessions');
    fireEvent.change(box, { target: { value: 'deploy' } });
    expect(screen.getByText('deploy checklist')).toBeInTheDocument();
    expect(screen.queryByText('cost spike triage')).not.toBeInTheDocument();
    fireEvent.change(box, { target: { value: 'zzz' } });
    expect(screen.getByText('common:assistant.noSessionsMatch:zzz')).toBeInTheDocument();
  });

  it('loads the clicked conversation', () => {
    const load = vi.fn();
    render(<SessionHistory showHistory sessions={sessions} loadSession={load} removeSession={noop} />);
    fireEvent.click(screen.getByText('cost spike triage'));
    expect(load).toHaveBeenCalledWith('w1');
  });
});
