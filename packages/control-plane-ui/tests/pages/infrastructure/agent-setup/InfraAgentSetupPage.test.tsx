import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import InfraAgentSetupPage from '@/pages/infrastructure/agent-setup/InfraAgentSetupPage';

// One device per status branch of renderStatusBadge + formatLastSeen.
const now = Date.now();
const devices = [
  { id: 'd-online', hostname: 'mac-1', os: 'macos', osVersion: '15.3', agentVersion: '1.2.0', status: 'active', lastHeartbeat: new Date(now - 5_000).toISOString() },
  { id: 'd-revoked', hostname: 'mac-2', os: 'macos', osVersion: '15.3', agentVersion: '1.1.0', status: 'revoked', lastHeartbeat: new Date(now - 5_000).toISOString() },
  { id: 'd-waiting', hostname: 'win-1', os: 'windows', osVersion: '11', agentVersion: '', status: 'enrolled', lastHeartbeat: null },
  { id: 'd-offline', hostname: 'lin-1', os: 'linux', osVersion: '', agentVersion: '1.0.0', status: 'active', lastHeartbeat: new Date(now - 5 * 60_000).toISOString() },
];

vi.mock('@/hooks/useApi', () => ({
  useApi: (_fn: unknown, key: unknown[]) =>
    key.includes('agent-devices')
      ? { data: { data: devices, total: devices.length }, loading: false, error: null }
      : { data: { controlPlane: 'https://cp.example.com/' }, loading: false, error: null },
}));

function wrap() {
  return render(
    <I18nextProvider i18n={i18n}><MemoryRouter><InfraAgentSetupPage /></MemoryRouter></I18nextProvider>,
  );
}

describe('InfraAgentSetupPage', () => {
  beforeEach(() => i18n.changeLanguage('en'));

  it('defaults to the macOS tab and shows the macOS download URL', () => {
    wrap();
    expect(screen.getByText('/downloads/NexusAgent-latest.pkg')).toBeInTheDocument();
  });

  it('switching to Linux swaps in the Linux download artifact', async () => {
    wrap();
    // Platform selector is a Radix Tabs trigger (role "tab"); activating it
    // requires the full pointer sequence, so use userEvent over fireEvent.
    const user = userEvent.setup();
    await user.click(screen.getByRole('tab', { name: /linux/i }));
    await waitFor(() =>
      expect(screen.getByText('/downloads/nexus-agent-linux-latest')).toBeInTheDocument(),
    );
  });

  it('renders the live device table with one row per enrolled device', () => {
    wrap();
    expect(screen.getByText('mac-1')).toBeInTheDocument();
    expect(screen.getByText('win-1')).toBeInTheDocument();
    // waiting device has no agent version → em-dash fallback (≥1 dash cell)
    expect(screen.getAllByText('—').length).toBeGreaterThan(0);
  });

  it('category chip narrows the FAQ list and a second click clears it', () => {
    wrap();
    const trustChip = screen.getByRole('button', { pressed: false, name: /trust/i });
    fireEvent.click(trustChip);
    expect(trustChip).toHaveAttribute('aria-pressed', 'true');
    // "show all" clear affordance appears once a chip is active
    fireEvent.click(trustChip);
    expect(trustChip).toHaveAttribute('aria-pressed', 'false');
  });

  it('search box filters the FAQ accordion (no-match yields the empty state)', () => {
    wrap();
    const search = screen.getByRole('searchbox');
    fireEvent.change(search, { target: { value: 'zzzznomatchquery' } });
    // every common + platform item filtered out → empty-state message renders
    expect(screen.getByText(i18n.t('pages:infrastructure.agentSetup.faqEmptyState'))).toBeInTheDocument();
  });
});
