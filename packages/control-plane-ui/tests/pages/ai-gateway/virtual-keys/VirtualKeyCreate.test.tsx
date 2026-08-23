import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { setDisplayTZ } from '@/lib/format';
import { VirtualKeyCreate } from '@/pages/ai-gateway/virtual-keys/VirtualKeyCreate';
import { expiryBounds } from '@/pages/ai-gateway/virtual-keys/expiryBounds';

/**
 * The clock and the display zone are pinned so every expiry assertion below is
 * an exact calendar date. The chosen instant sits early-morning east of UTC —
 * the admin is already on 2026-07-16 while the instant is still on 2026-07-15
 * in UTC — so a UTC-anchored picker floor or default would be visibly off by a
 * day rather than coincidentally correct.
 */
const FROZEN_NOW = '2026-07-15T19:30:00Z';
const DISPLAY_TZ = 'Asia/Shanghai';
const LOCAL_TODAY = '2026-07-16';
const LOCAL_TOMORROW = '2026-07-17';
const LOCAL_ONE_MONTH_OUT = '2026-08-16';

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
    // shouldAdvanceTime keeps userEvent / waitFor progressing under a pinned clock.
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(new Date(FROZEN_NOW));
    setDisplayTZ(DISPLAY_TZ);
    apiByKey.models = ok({ data: [] });
    apiByKey.projects = ok({ data: [{ id: 'p1', name: 'Proj', organization: { id: 'o1', name: 'OrgA' } }] });
    svc.virtualKeyApi.create.mockResolvedValue({ key: 'nx_secret_plain', id: 'vk1' });
  });

  afterEach(() => {
    vi.useRealTimers();
    setDisplayTZ(null);
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

  it('pre-fills the expiration date input with one month from the local today', () => {
    wrap();
    const dateInput = screen.getByDisplayValue(/^\d{4}-\d{2}-\d{2}$/);
    expect((dateInput as HTMLInputElement).value).toBe(LOCAL_ONE_MONTH_OUT);
  });

  /* ── Expiration: max attribute caps at ~3 months ────────────────────── */

  it('leaves the expiration date input unbounded above', () => {
    // The server only requires a FUTURE expiry (requireApplicationExpiry); it
    // imposes no ceiling. A max here would silently re-impose the removed
    // 3-month cap and block a legitimate date.
    wrap();
    const dateInput = screen.getByDisplayValue(/^\d{4}-\d{2}-\d{2}$/);
    expect((dateInput as HTMLInputElement).max).toBe('');
    expect('max' in expiryBounds()).toBe(false);
  });

  it('sets min on the expiration date input to tomorrow on the local calendar', () => {
    wrap();
    const dateInput = screen.getByDisplayValue(/^\d{4}-\d{2}-\d{2}$/);
    const minAttr = (dateInput as HTMLInputElement).min;
    expect(minAttr).toBe(LOCAL_TOMORROW);
    // The admin's own today must not be selectable: requireApplicationExpiry
    // rejects an expiry that is not in the future.
    expect(minAttr > LOCAL_TODAY).toBe(true);
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
        // The pre-filled local day, stamped to its last moment in the display
        // zone: end of 2026-08-16 at +08 is 15:59:59.999Z the same day. Create
        // binds into a Go time.Time and takes RFC3339 only, so a bare date 400s.
        expiresAt: '2026-08-16T15:59:59.999Z',
      }),
    ));
    // No neverExpires key in the call
    expect(svc.virtualKeyApi.create).not.toHaveBeenCalledWith(
      expect.objectContaining({ neverExpires: expect.anything() }),
    );
    await waitFor(() => expect(screen.getByText('nx_secret_plain')).toBeInTheDocument());
  });
});

/**
 * Bulk selection in the model-access picker.
 *
 * The rule these pin: a bulk action may only touch what the admin can SEE.
 * "Select all" used to grant every model in the catalogue regardless of the
 * search box — filter to "claude", click it, and the key silently gained every
 * OpenAI model too. That makes a key's real permissions differ from the list
 * being looked at, which is the same defect that made glob refs unacceptable,
 * one layer up.
 */
describe('VirtualKeyCreate — model-access bulk selection', () => {
  const GROUPS = [
    {
      provider: { id: 'prov-anthropic', name: 'anthropic', displayName: 'ANTHROPIC' },
      models: [
        { id: 'm-haiku', name: 'Claude Haiku 4.5', code: 'claude-haiku-4-5' },
        { id: 'm-opus', name: 'Claude Opus 4.5', code: 'claude-opus-4-5' },
      ],
    },
    {
      provider: { id: 'prov-openai', name: 'openai', displayName: 'OPENAI' },
      models: [{ id: 'm-gpt', name: 'GPT 5', code: 'gpt-5' }],
    },
  ];

  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers({ shouldAdvanceTime: true });
    vi.setSystemTime(new Date(FROZEN_NOW));
    setDisplayTZ(DISPLAY_TZ);
    apiByKey.models = ok({ data: GROUPS });
    apiByKey.projects = ok({ data: [{ id: 'p1', name: 'Proj', organization: { id: 'o1', name: 'OrgA' } }] });
    svc.virtualKeyApi.create.mockResolvedValue({ key: 'nx_secret_plain', id: 'vk1' });
  });
  afterEach(() => {
    vi.useRealTimers();
    setDisplayTZ(null);
  });

  const checkedCodes = () =>
    (screen.getAllByRole('checkbox') as HTMLInputElement[])
      .filter(c => c.checked)
      .length;

  it('select all with a search filter active grants only the visible models', async () => {
    wrap();
    fireEvent.change(screen.getByPlaceholderText(i18n.t('pages:virtualKeys.searchModels')), {
      target: { value: 'claude' },
    });
    // Only the two Anthropic rows are rendered now.
    expect(screen.getAllByRole('checkbox')).toHaveLength(2);

    fireEvent.click(screen.getAllByText(i18n.t('pages:virtualKeys.selectAll'))[0]);

    // Clearing the filter must not reveal a third, invisibly-granted model.
    fireEvent.change(screen.getByPlaceholderText(i18n.t('pages:virtualKeys.searchModels')), {
      target: { value: '' },
    });
    await waitFor(() => expect(screen.getAllByRole('checkbox')).toHaveLength(3));
    expect(checkedCodes()).toBe(2);
  });

  it('deselect all with a filter active leaves hidden selections alone', async () => {
    wrap();
    // Tick the OpenAI model while everything is visible.
    fireEvent.click(screen.getByLabelText(/GPT 5/i));
    expect(checkedCodes()).toBe(1);

    fireEvent.change(screen.getByPlaceholderText(i18n.t('pages:virtualKeys.searchModels')), {
      target: { value: 'claude' },
    });
    fireEvent.click(screen.getAllByText(i18n.t('pages:virtualKeys.deselectAll'))[0]);

    fireEvent.change(screen.getByPlaceholderText(i18n.t('pages:virtualKeys.searchModels')), {
      target: { value: '' },
    });
    // The hidden GPT selection survives a bulk action aimed at Anthropic.
    await waitFor(() => expect(checkedCodes()).toBe(1));
  });

  it('the per-provider control selects that provider only, then clears it', async () => {
    wrap();
    // The first per-provider button belongs to ANTHROPIC (2 models).
    const perProvider = screen.getAllByText(i18n.t('pages:virtualKeys.selectAll'));
    fireEvent.click(perProvider[1]);
    await waitFor(() => expect(checkedCodes()).toBe(2));

    // It is a toggle: fully-selected flips to deselect.
    fireEvent.click(screen.getAllByText(i18n.t('pages:virtualKeys.deselectAll'))[1]);
    await waitFor(() => expect(checkedCodes()).toBe(0));
  });
});
