import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { VirtualKeyCreate } from '@/pages/ai-gateway/virtual-keys/VirtualKeyCreate';
import { expiryBounds } from '@/pages/ai-gateway/virtual-keys/expiryBounds';

const svc = vi.hoisted(() => ({
  virtualKeyApi: { create: vi.fn() },
  projectApi: { list: vi.fn() },
  systemApi: { listModels: vi.fn() },
}));
vi.mock('@/api/services', () => svc);
vi.mock('react-router-dom', async (orig) => ({ ...(await orig<typeof import('react-router-dom')>()), useNavigate: () => vi.fn() }));
vi.mock('@/hooks/useMutation', () => ({
  useMutation: (fn: (a: unknown) => Promise<unknown>, opts?: { onSuccess?: (r: unknown) => void }) => ({
    mutate: async (arg: unknown) => { const r = await fn(arg); opts?.onSuccess?.(r); return r; },
    loading: false,
  }),
}));
const apiByKey = vi.hoisted(() => ({ models: undefined as unknown, projects: undefined as unknown }));
vi.mock('@/hooks/useApi', () => ({
  useApi: (_fn: unknown, key: unknown[]) => (key.includes('projects') ? apiByKey.projects : apiByKey.models),
}));

function ok(d: unknown) { return { data: d, loading: false, error: null, refetch: vi.fn() }; }
function wrap() {
  return render(<I18nextProvider i18n={i18n}><MemoryRouter><VirtualKeyCreate /></MemoryRouter></I18nextProvider>);
}
const createLabel = () => i18n.t('pages:virtualKeys.createVirtualKey');

describe('VirtualKeyCreate', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    apiByKey.models = ok({ data: [] });
    apiByKey.projects = ok({ data: [{ id: 'p1', name: 'Proj', organization: { id: 'o1', name: 'OrgA' } }] });
    svc.virtualKeyApi.create.mockResolvedValue({ key: 'nx_secret_plain', id: 'vk1' });
  });

  it('renders the create form with the name field', () => {
    wrap();
    expect(screen.getByPlaceholderText(i18n.t('pages:virtualKeys.namePlaceholder'))).toBeInTheDocument();
  });

  it('does not submit when the name is empty (zod required)', async () => {
    wrap();
    fireEvent.click(screen.getByRole('button', { name: createLabel() }));
    await new Promise((r) => setTimeout(r, 50));
    expect(svc.virtualKeyApi.create).not.toHaveBeenCalled();
  });

  /* ── Expiration: no Never-expires affordance (application VKs) ───────── */

  it('does not render a "Never expires" checkbox for application VKs', () => {
    wrap();
    expect(screen.queryByText(i18n.t('pages:virtualKeys.neverExpires'))).not.toBeInTheDocument();
    // There is no checkbox labeled "Never expires"
    const checkboxes = screen.queryAllByRole('checkbox');
    expect(checkboxes).toHaveLength(0);
  });

  /* ── Expiration: default pre-filled to ~1 month out ─────────────────── */

  it('pre-fills the expiration date input with a value ~1 month from now', () => {
    wrap();
    const dateInput = screen.getByDisplayValue(/^\d{4}-\d{2}-\d{2}$/);
    const value = (dateInput as HTMLInputElement).value;
    // Value must be a YYYY-MM-DD string
    expect(value).toMatch(/^\d{4}-\d{2}-\d{2}$/);
    // Must be in the future (at least today)
    const chosenMs = new Date(`${value}T00:00:00Z`).getTime();
    expect(chosenMs).toBeGreaterThan(Date.now() - 24 * 60 * 60 * 1000);
    // Must be approximately 1 month from now (within a 5-day window)
    const oneMonthMs = (() => { const d = new Date(); d.setMonth(d.getMonth() + 1); return d.getTime(); })();
    expect(Math.abs(chosenMs - oneMonthMs)).toBeLessThan(5 * 24 * 60 * 60 * 1000);
  });

  /* ── Expiration: max attribute caps at ~3 months ────────────────────── */

  it('sets max on the expiration date input to within 3 months from now', () => {
    wrap();
    const dateInput = screen.getByDisplayValue(/^\d{4}-\d{2}-\d{2}$/);
    const maxAttr = (dateInput as HTMLInputElement).max;
    expect(maxAttr).toBeTruthy();
    const maxMs = new Date(`${maxAttr}T00:00:00Z`).getTime();
    const threeMonths = new Date();
    threeMonths.setMonth(threeMonths.getMonth() + 3);
    // max must be strictly before the server ceiling of now+3months
    expect(maxMs).toBeLessThan(threeMonths.getTime());
    // max must be in the future
    expect(maxMs).toBeGreaterThan(Date.now());
    // Consistency: matches expiryBounds().max
    const { max } = expiryBounds();
    expect(maxAttr).toBe(max);
  });

  it('sets min on the expiration date input to tomorrow', () => {
    wrap();
    const dateInput = screen.getByDisplayValue(/^\d{4}-\d{2}-\d{2}$/);
    const minAttr = (dateInput as HTMLInputElement).min;
    expect(minAttr).toBeTruthy();
    // min must be tomorrow or later (not today)
    const minMs = new Date(`${minAttr}T00:00:00Z`).getTime();
    expect(minMs).toBeGreaterThan(Date.now() - 24 * 60 * 60 * 1000);
    // Matches expiryBounds().min
    const { min } = expiryBounds();
    expect(minAttr).toBe(min);
  });

  /* ── Project field: required asterisk ───────────────────────────────── */

  it('renders a required asterisk (*) next to the Project label', () => {
    wrap();
    const projectLabel = screen.getByText(i18n.t('pages:virtualKeys.project'), { selector: 'label' });
    // The asterisk is rendered as a <span> child of the label with " *" text
    const asterisk = projectLabel.querySelector('span[aria-hidden="true"]');
    expect(asterisk).not.toBeNull();
    expect(asterisk!.textContent).toContain('*');
  });

  /* ── Submit wires expiresAt to RFC3339 stamped date ─────────────────── */

  it('submits an application VK with a stamped expiresAt and no neverExpires field', async () => {
    const user = userEvent.setup();
    wrap();
    const nameInput = screen.getByPlaceholderText(i18n.t('pages:virtualKeys.namePlaceholder'));
    await user.type(nameInput, 'prod-key');
    // Select a project so the form is valid
    const projectSelect = screen.getByRole('combobox');
    await user.selectOptions(projectSelect, 'p1');
    // Submit
    fireEvent.submit(nameInput.closest('form')!);
    await waitFor(() => expect(svc.virtualKeyApi.create).toHaveBeenCalledWith(
      expect.objectContaining({
        name: 'prod-key',
        vkType: 'application',
        enabled: true,
        // expiresAt is always a stamped RFC3339 string — never undefined
        expiresAt: expect.stringMatching(/^\d{4}-\d{2}-\d{2}T23:59:59Z$/),
      }),
    ));
    // No neverExpires key in the call
    expect(svc.virtualKeyApi.create).not.toHaveBeenCalledWith(
      expect.objectContaining({ neverExpires: expect.anything() }),
    );
    await waitFor(() => expect(screen.getByText('nx_secret_plain')).toBeInTheDocument());
  });
});
