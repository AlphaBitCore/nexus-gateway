import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { StepProvider } from '@/pages/setup/steps/StepProvider';

const svc = vi.hoisted(() => ({
  providerApi: { getTemplates: vi.fn(), create: vi.fn() },
  credentialApi: { create: vi.fn() },
}));
vi.mock('@/api/services', () => svc);
const addToast = vi.fn();
vi.mock('@/context/ToastContext', () => ({ useToast: () => ({ addToast }) }));

const templates = [{ name: 'openai', displayName: 'OpenAI', description: 'd', baseUrl: 'https://api.openai.com', adapterType: 'openai' }];
const createBtn = () => screen.getByRole('button', { name: i18n.t('pages:setup.createProvider', 'Create Provider') });

function wrap(result: Record<string, unknown>, onRefresh = vi.fn()) {
  const r = render(<I18nextProvider i18n={i18n}><StepProvider result={result as never} onRefresh={onRefresh} /></I18nextProvider>);
  return { onRefresh, container: r.container };
}

describe('StepProvider', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    svc.providerApi.getTemplates.mockResolvedValue({ data: templates });
    svc.providerApi.create.mockResolvedValue({ id: 'p1' });
    svc.credentialApi.create.mockResolvedValue({});
  });

  it('renders the loading state', () => {
    wrap({ status: 'loading', data: undefined });
    expect(screen.getByText(i18n.t('common:loading'))).toBeInTheDocument();
  });

  it('renders detected providers when complete', () => {
    wrap({ status: 'complete', data: { providers: [{ id: 'p1', name: 'openai', displayName: 'OpenAI', enabled: true }], credentials: [{ id: 'c1', providerId: 'p1' }] } });
    expect(screen.getByText('OpenAI')).toBeInTheDocument();
  });

  it('validates a missing template selection', async () => {
    wrap({ status: 'incomplete', data: { providers: [], credentials: [] } });
    await waitFor(() => expect(svc.providerApi.getTemplates).toHaveBeenCalled());
    fireEvent.click(createBtn());
    expect(addToast).toHaveBeenCalledWith(i18n.t('pages:setup.selectTemplate', 'Select a provider template'), 'error');
    expect(svc.providerApi.create).not.toHaveBeenCalled();
  });

  // The combobox always renders two static options ("-- Select --" and
  // "Custom provider"), so an options.length check passes before the async
  // template fetch lands; wait for the template's own <option> instead —
  // changing the select to a value with no matching option is a no-op and
  // Create then takes the "select a template" validation branch.
  const waitForTemplateOption = () =>
    waitFor(() => expect(screen.getByRole('combobox').querySelector('option[value=openai]')).toBeTruthy(), { timeout: 5000 });

  it('validates a missing API key once a template is chosen', async () => {
    const { container } = wrap({ status: 'incomplete', data: { providers: [], credentials: [] } });
    await waitForTemplateOption();
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'openai' } });
    // Wait for the selection to commit before clicking Create — otherwise
    // validation runs against a not-yet-ready form and the toast never fires.
    // The extended waitFor timeouts give a busy CI runner headroom over the
    // 1000 ms default.
    await waitFor(() => expect((screen.getByRole('combobox') as HTMLSelectElement).value).toBe('openai'), { timeout: 5000 });
    await waitFor(() => expect(container.querySelector('input[type=password]')).toBeTruthy(), { timeout: 5000 });
    fireEvent.click(createBtn());
    await waitFor(
      () => expect(addToast).toHaveBeenCalledWith(i18n.t('pages:setup.apiKeyRequired', 'API Key is required'), 'error'),
      { timeout: 5000 },
    );
    expect(container.querySelector('input[type=password]')).toBeTruthy();
  }, 30_000);

  it('creates a provider + credential from the selected template', async () => {
    const { container, onRefresh } = wrap({ status: 'incomplete', data: { providers: [], credentials: [] } });
    await waitForTemplateOption();
    fireEvent.change(screen.getByRole('combobox'), { target: { value: 'openai' } });
    // Wait for the selection to commit before clicking Create — clicking while
    // the select is still empty takes the "select a template" validation branch
    // and providerApi.create is never called.
    await waitFor(() => expect((screen.getByRole('combobox') as HTMLSelectElement).value).toBe('openai'), { timeout: 5000 });
    fireEvent.change(container.querySelector('input[type=password]')!, { target: { value: 'sk-abc' } });
    fireEvent.click(createBtn());
    // Generous timeouts: under the full parallel CI run the click→create chain
    // (select state-update → provider create → credential create → refresh)
    // can exceed waitFor's 1000ms default.
    await waitFor(() => expect(svc.providerApi.create).toHaveBeenCalledWith(expect.objectContaining({ name: 'openai', adapterType: 'openai', enabled: true })), { timeout: 5000 });
    await waitFor(() => expect(svc.credentialApi.create).toHaveBeenCalledWith(expect.objectContaining({ providerId: 'p1', apiKey: 'sk-abc' })), { timeout: 5000 });
    await waitFor(() => expect(onRefresh).toHaveBeenCalled(), { timeout: 5000 });
  }, 30_000);
});
