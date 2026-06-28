import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { VirtualKeyInfoTab, type VirtualKeyInfoTabProps } from '@/pages/ai-gateway/virtual-keys/detail/VirtualKeyInfoTab';
import { expiryBounds } from '@/pages/ai-gateway/virtual-keys/expiryBounds';

const vk = {
  id: 'vk-1', name: 'prod-key', keyPrefix: 'nx_abc', sourceApp: 'cli',
  enabled: true, expiresAt: null, createdAt: '2026-05-01T00:00:00Z',
  allowedModels: [{ providerId: 'p-openai', modelId: 'gpt-4o' }],
} as never;
const project = { id: 'proj-1', name: 'Acme', organization: { id: 'o-1', name: 'AcmeOrg' } } as never;
const modelsData = {
  data: [
    { provider: { id: 'p-openai', name: 'openai', displayName: 'OpenAI' }, models: [{ id: 'gpt-4o', name: 'GPT-4o' }, { id: 'gpt-4o-mini', name: 'GPT-4o mini' }] },
  ],
} as never;

function baseProps(overrides: Partial<VirtualKeyInfoTabProps> = {}): VirtualKeyInfoTabProps {
  return {
    vk, project, modelsData, projectsData: { data: [project] },
    regenConfirming: false, setRegenConfirming: vi.fn(),
    newKey: null, keyCopied: false, regenerateKey: vi.fn(), regenerating: false,
    copyNewKey: vi.fn(), dismissNewKey: vi.fn(),
    isEditing: false,
    editProjectId: '', setEditProjectId: vi.fn(),
    editSourceApp: 'cli', setEditSourceApp: vi.fn(),
    editEnabled: true, setEditEnabled: vi.fn(),
    editRateLimitRpm: '60', setEditRateLimitRpm: vi.fn(),
    editSelectedModels: [], setEditSelectedModels: vi.fn(),
    editExpiresAt: '', setEditExpiresAt: vi.fn(),
    editNeverExpires: false, setEditNeverExpires: vi.fn(),
    updating: false, handleSave: vi.fn(), cancelEditing: vi.fn(),
    ...overrides,
  } as VirtualKeyInfoTabProps;
}

function wrap(props: VirtualKeyInfoTabProps) {
  return render(
    <I18nextProvider i18n={i18n}><MemoryRouter><VirtualKeyInfoTab {...props} /></MemoryRouter></I18nextProvider>,
  );
}

describe('VirtualKeyInfoTab — view mode', () => {
  it('renders the key info grid, masked secret, project link, and allowed-model chips', () => {
    wrap(baseProps());
    expect(screen.getByText('prod-key')).toBeInTheDocument();
    expect(screen.getByText('nx_abc••••••••')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Acme' })).toHaveAttribute('href', '/iam/projects/proj-1');
    // allowed model resolves provider label + model name
    expect(screen.getByText('OpenAI')).toBeInTheDocument();
    expect(screen.getByText('GPT-4o')).toBeInTheDocument();
  });

  it('regenerate button arms the confirm step', () => {
    const setRegenConfirming = vi.fn();
    wrap(baseProps({ setRegenConfirming }));
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:virtualKeys.regenerateSecretKey') }));
    expect(setRegenConfirming).toHaveBeenCalledWith(true);
  });

  it('confirm-regenerate state fires regenerateKey', () => {
    const regenerateKey = vi.fn();
    wrap(baseProps({ regenConfirming: true, regenerateKey }));
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:virtualKeys.confirmRegenerate') }));
    expect(regenerateKey).toHaveBeenCalledWith(undefined);
  });

  it('a freshly minted key offers copy + dismiss', () => {
    const copyNewKey = vi.fn();
    const dismissNewKey = vi.fn();
    wrap(baseProps({ newKey: 'nx_secret_plaintext', copyNewKey, dismissNewKey }));
    expect(screen.getByText('nx_secret_plaintext')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:virtualKeys.copy') }));
    expect(copyNewKey).toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:virtualKeys.dismiss') }));
    expect(dismissNewKey).toHaveBeenCalled();
  });
});

describe('VirtualKeyInfoTab — edit mode + GroupedModelSelect', () => {
  it('renders the edit form with the grouped model selector', () => {
    wrap(baseProps({ isEditing: true }));
    expect(screen.getByText(i18n.t('pages:virtualKeys.editVirtualKey'))).toBeInTheDocument();
    expect(screen.getByText('GPT-4o mini')).toBeInTheDocument();
  });

  it('checking a model adds its provider+model ref', () => {
    const setEditSelectedModels = vi.fn();
    wrap(baseProps({ isEditing: true, setEditSelectedModels }));
    const row = screen.getByText('GPT-4o mini').closest('label')!;
    fireEvent.click(within(row).getByRole('checkbox'));
    expect(setEditSelectedModels).toHaveBeenCalledWith([{ providerId: 'p-openai', modelId: 'gpt-4o-mini' }]);
  });

  it('Select all picks every model ref; Deselect all clears', () => {
    const setEditSelectedModels = vi.fn();
    wrap(baseProps({ isEditing: true, setEditSelectedModels }));
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:virtualKeys.selectAll') }));
    expect(setEditSelectedModels).toHaveBeenCalledWith([
      { providerId: 'p-openai', modelId: 'gpt-4o' },
      { providerId: 'p-openai', modelId: 'gpt-4o-mini' },
    ]);
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:virtualKeys.deselectAll') }));
    expect(setEditSelectedModels).toHaveBeenCalledWith([]);
  });

  it('model search filters the selector list', () => {
    wrap(baseProps({ isEditing: true }));
    const search = screen.getByPlaceholderText(i18n.t('pages:virtualKeys.searchModels'));
    fireEvent.change(search, { target: { value: 'mini' } });
    expect(screen.getByText('GPT-4o mini')).toBeInTheDocument();
    expect(screen.queryByText('GPT-4o', { exact: true })).not.toBeInTheDocument();
  });

  it('Save + Cancel wire to their handlers', () => {
    const handleSave = vi.fn();
    const cancelEditing = vi.fn();
    wrap(baseProps({ isEditing: true, handleSave, cancelEditing }));
    fireEvent.click(screen.getByRole('button', { name: i18n.t('pages:virtualKeys.saveChanges') }));
    fireEvent.click(screen.getByRole('button', { name: i18n.t('common:cancel') }));
    expect(handleSave).toHaveBeenCalled();
    expect(cancelEditing).toHaveBeenCalled();
  });

  /* ── Expiration: no Never-expires affordance (application VKs) ───────── */

  it('does not render a "Never expires" checkbox in edit mode', () => {
    wrap(baseProps({ isEditing: true }));
    expect(screen.queryByText(i18n.t('pages:virtualKeys.neverExpires'))).not.toBeInTheDocument();
    // Only checkboxes come from the model selector — none for never-expires
    const checkboxes = screen.queryAllByRole('checkbox');
    // No never-expires checkbox: the model checkboxes have labels like "GPT-4o"
    for (const cb of checkboxes) {
      const label = cb.closest('label');
      expect(label?.textContent).not.toContain('never');
      expect(label?.textContent?.toLowerCase()).not.toContain('expires');
    }
  });

  /* ── Expiration: max attribute caps at ~3 months ────────────────────── */

  it('sets max on the expiration date input to within 3 months from now', () => {
    wrap(baseProps({ isEditing: true, editExpiresAt: '2026-09-01' }));
    // The date input has type="date" and the editExpiresAt value is shown
    const dateInput = screen.getByDisplayValue('2026-09-01') as HTMLInputElement;
    const maxAttr = dateInput.max;
    expect(maxAttr).toBeTruthy();
    const maxMs = new Date(`${maxAttr}T00:00:00Z`).getTime();
    const threeMonths = new Date();
    threeMonths.setMonth(threeMonths.getMonth() + 3);
    expect(maxMs).toBeLessThan(threeMonths.getTime());
    expect(maxAttr).toBe(expiryBounds().max);
  });

  it('sets min on the expiration date input to tomorrow', () => {
    wrap(baseProps({ isEditing: true, editExpiresAt: '2026-09-01' }));
    const dateInput = screen.getByDisplayValue('2026-09-01') as HTMLInputElement;
    expect(dateInput.min).toBe(expiryBounds().min);
  });

  /* ── Project field: required asterisk ───────────────────────────────── */

  it('renders a required asterisk (*) next to the Project label in edit mode', () => {
    wrap(baseProps({ isEditing: true }));
    const projectLabel = screen.getByText(i18n.t('pages:virtualKeys.project'), { selector: 'label' });
    const asterisk = projectLabel.querySelector('span[aria-hidden="true"]');
    expect(asterisk).not.toBeNull();
    expect(asterisk!.textContent).toContain('*');
  });
});
