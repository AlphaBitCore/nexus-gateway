import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { LiveTrafficBasicFilters } from '@/pages/traffic/filters/LiveTrafficBasicFilters';
import { EMPTY_LIVE_TRAFFIC_FILTERS } from '@/pages/traffic/filters/liveTrafficFilters';

// The org/provider/vk comboboxes call these loaders on open; resolve empty so
// nothing hits the network even if a combobox eagerly fetches.
vi.mock('@/api/services', () => {
  const fn = () => Promise.resolve({ data: [], total: 0 });
  const anyApi = new Proxy({}, { get: () => fn });
  return {
    projectApi: anyApi, providerApi: anyApi, systemApi: anyApi,
    virtualKeyApi: anyApi, devicesApi: anyApi, iamApi: anyApi, hubApi: anyApi,
  };
});

function wrap(source: '' | 'vk' | 'proxy' | 'agent', patch = {}, onPatch = vi.fn()) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <I18nextProvider i18n={i18n}>
        <LiveTrafficBasicFilters value={{ ...EMPTY_LIVE_TRAFFIC_FILTERS, ...patch }} onPatch={onPatch} source={source} />
      </I18nextProvider>
    </QueryClientProvider>,
  );
  return onPatch;
}

describe('LiveTrafficBasicFilters', () => {
  beforeEach(() => i18n.changeLanguage('en'));

  it('vk source gates the model picker on a selected provider', () => {
    // no provider chosen → model picker disabled with the "select provider first" hint
    wrap('vk', { _providerId: '' });
    expect(screen.getByPlaceholderText(i18n.t('pages:traffic.placeholderModelDisabled'))).toBeInTheDocument();
  });

  it('vk source enables the model picker once a provider is set', () => {
    wrap('vk', { _providerId: 'prov-1', provider: 'openai' });
    expect(screen.getByPlaceholderText(i18n.t('pages:traffic.placeholderModelEnabled'))).toBeInTheDocument();
  });

  it('quick-range chips patch a computed start+end window', () => {
    const onPatch = wrap('vk');
    fireEvent.click(screen.getByText('1h'));
    expect(onPatch).toHaveBeenCalledWith(expect.objectContaining({ startTime: expect.any(String), endTime: expect.any(String) }));
    onPatch.mockClear();
    fireEvent.click(screen.getByText('7d'));
    expect(onPatch).toHaveBeenCalledWith(expect.objectContaining({ startTime: expect.any(String), endTime: expect.any(String) }));
  });

  it('marks the 1h chip active when the window is exactly one hour', () => {
    const end = new Date();
    const start = new Date(end.getTime() - 3600_000);
    wrap('vk', { startTime: start.toISOString(), endTime: end.toISOString() });
    expect(screen.getByRole('button', { name: '1h' })).toHaveAttribute('aria-pressed', 'true');
  });

  it('does not render the removed clear-time affordance', () => {
    wrap('vk', { startTime: '2026-05-28T00:00', endTime: '2026-05-28T01:00' });
    expect(screen.queryByRole('button', { name: i18n.t('pages:traffic.clearTime') })).not.toBeInTheDocument();
  });

  it('vk source exposes the request hook-decision filter', () => {
    const onPatch = wrap('vk');
    const select = screen.getByDisplayValue(i18n.t('pages:traffic.optionAll'));
    fireEvent.change(select, { target: { value: 'REJECT_HARD' } });
    expect(onPatch).toHaveBeenCalledWith({ requestHookDecision: 'REJECT_HARD' });
  });

  it('proxy source exposes the TLS bump-status filter', () => {
    const onPatch = wrap('proxy');
    const select = screen.getByDisplayValue(i18n.t('pages:traffic.optionAny'));
    fireEvent.change(select, { target: { value: 'BUMP_SUCCESS' } });
    expect(onPatch).toHaveBeenCalledWith({ bumpStatus: 'BUMP_SUCCESS' });
  });

  it('24h quick-range chip patches a one-day window', () => {
    const onPatch = wrap('vk');
    fireEvent.click(screen.getByText('24h'));
    expect(onPatch).toHaveBeenCalledWith(expect.objectContaining({ startTime: expect.any(String), endTime: expect.any(String) }));
  });

  it('agent source exposes the source-process + target-host + path filters', () => {
    const onPatch = wrap('agent');
    fireEvent.change(screen.getByLabelText(i18n.t('pages:traffic.labelSourceProcess')), { target: { value: 'curl' } });
    expect(onPatch).toHaveBeenCalledWith({ sourceProcess: 'curl' });
    fireEvent.change(screen.getByLabelText(i18n.t('pages:traffic.labelTargetHost')), { target: { value: 'api.openai.com' } });
    expect(onPatch).toHaveBeenCalledWith({ targetHost: 'api.openai.com' });
    fireEvent.change(screen.getByLabelText(i18n.t('pages:traffic.labelPath')), { target: { value: '/v1/chat' } });
    expect(onPatch).toHaveBeenCalledWith({ path: '/v1/chat' });
  });

  it('all-sources variant exposes the hook-decision + target-host filters', () => {
    const onPatch = wrap('');
    fireEvent.change(screen.getByLabelText(i18n.t('pages:traffic.labelHookDecision')), { target: { value: 'REJECT_HARD' } });
    expect(onPatch).toHaveBeenCalledWith({ requestHookDecision: 'REJECT_HARD' });
    fireEvent.change(screen.getByLabelText(i18n.t('pages:traffic.labelTargetHost')), { target: { value: 'example.com' } });
    expect(onPatch).toHaveBeenCalledWith({ targetHost: 'example.com' });
  });

  it('proxy source target-host + path inputs patch their fields', () => {
    const onPatch = wrap('proxy');
    fireEvent.change(screen.getByLabelText(i18n.t('pages:traffic.labelTargetHost')), { target: { value: 'host.tld' } });
    expect(onPatch).toHaveBeenCalledWith({ targetHost: 'host.tld' });
    fireEvent.change(screen.getByLabelText(i18n.t('pages:traffic.labelPath')), { target: { value: '/p' } });
    expect(onPatch).toHaveBeenCalledWith({ path: '/p' });
  });
});
