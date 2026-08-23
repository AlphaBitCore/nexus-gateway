import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor, fireEvent, act } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { SearchableCombobox, type ComboboxOption } from '../../../../src/components/ui/SearchableCombobox/SearchableCombobox';

const OPTS: ComboboxOption[] = [
  { id: '1', label: 'Alpha' },
  { id: '2', label: 'Beta' },
  { id: '3', label: 'Gamma' },
];

function setup(over: Partial<React.ComponentProps<typeof SearchableCombobox>> = {}) {
  const onSelect = vi.fn();
  const fetchOptions = vi.fn().mockResolvedValue(OPTS);
  const ui = (extra: Partial<React.ComponentProps<typeof SearchableCombobox>>) => (
    <I18nextProvider i18n={i18n}>
      <SearchableCombobox
        valueId=""
        valueLabel=""
        placeholder="search…"
        ariaLabel="picker"
        fetchOptions={fetchOptions}
        onSelect={onSelect}
        {...over}
        {...extra}
      />
    </I18nextProvider>
  );
  const view = render(ui({}));
  // Re-render as a PARENT would: same component, fresh prop identities. Every
  // call site in the app builds `fetchOptions` inline, so this is what a parent
  // render actually hands the combobox.
  const rerenderWith = (extra: Partial<React.ComponentProps<typeof SearchableCombobox>>) =>
    view.rerender(ui(extra));
  return { onSelect, fetchOptions, rerenderWith };
}

// Let every pending debounce fire and its fetch settle. Wrapped in act so the
// resulting state updates are flushed rather than warned about.
async function settleDebounce() {
  await act(async () => {
    await new Promise((r) => setTimeout(r, 400));
  });
}

describe('SearchableCombobox', () => {
  it('fetches (debounced) on type and renders options', async () => {
    const { fetchOptions } = setup();
    const input = screen.getByRole('combobox', { name: 'picker' });
    await userEvent.type(input, 'al');
    expect(await screen.findByRole('option', { name: 'Alpha' })).toBeInTheDocument();
    expect(fetchOptions).toHaveBeenCalledWith('al');
  });

  it('selects an option on click → onSelect + closes', async () => {
    const { onSelect } = setup();
    await userEvent.type(screen.getByRole('combobox', { name: 'picker' }), 'a');
    const opt = await screen.findByRole('option', { name: 'Beta' });
    await userEvent.click(opt);
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({ id: '2', label: 'Beta' }));
    expect(screen.queryByRole('listbox')).toBeNull();
  });

  it('clears the selection via the inline clear button', async () => {
    const { onSelect } = setup({ valueId: '2', valueLabel: 'Beta' });
    await userEvent.click(screen.getByRole('button', { name: i18n.t('common:clear') }));
    expect(onSelect).toHaveBeenCalledWith(null);
  });

  it('keyboard ArrowDown opens + highlights, Enter selects', async () => {
    const { onSelect } = setup({ allowEmptyQueryFetch: true });
    const input = screen.getByRole('combobox', { name: 'picker' });
    input.focus();
    await screen.findByRole('option', { name: 'Alpha' });
    // Focus with allowEmptyQueryFetch schedules a debounced fetch; findByRole
    // resolves as soon as the first result renders, but a still-pending refetch
    // can settle between the ArrowDown presses and Enter and reset the highlight
    // out from under the navigation (the exact race the next test documents).
    // Flush every pending debounce so the option list is stable before we drive
    // the keyboard — otherwise Enter intermittently selects the wrong option.
    await settleDebounce();
    fireEvent.keyDown(input, { key: 'ArrowDown' }); // highlight 0
    fireEvent.keyDown(input, { key: 'ArrowDown' }); // highlight 1
    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({ id: '2' }));
  });

  // A parent re-render must be inert. Every call site passes `fetchOptions` as
  // an inline arrow, so the prop identity changes on every parent render — and
  // the Live Traffic filters, which hold seven of these, sit on a page that
  // polls. A refetch per poll is both a request nobody asked for and the thing
  // that moves the user's keyboard highlight out from under them.
  it('does not refetch when the parent re-renders with a fresh fetchOptions identity', async () => {
    const seen: string[] = [];
    const inline = () => async (q: string) => {
      seen.push(q);
      return OPTS;
    };
    const { rerenderWith } = setup({ allowEmptyQueryFetch: true, fetchOptions: inline() });
    screen.getByRole('combobox', { name: 'picker' }).focus();
    await screen.findByRole('option', { name: 'Alpha' });
    expect(seen).toHaveLength(1);

    rerenderWith({ fetchOptions: inline() });
    await settleDebounce();
    expect(seen).toHaveLength(1);
  });

  // The highlight belongs to the user. A fetch that comes back with the same
  // choices has told us nothing new, so it must not move their cursor — the
  // failure mode is silent and lands on Enter: they are looking at row two and
  // select row one.
  it('keeps the highlight when a refetch returns the same options', async () => {
    const { onSelect } = setup({
      allowEmptyQueryFetch: true,
      // A fresh array each call, which is what any real fetch returns.
      fetchOptions: async () => OPTS.map((o) => ({ ...o })),
    });
    const input = screen.getByRole('combobox', { name: 'picker' });
    input.focus();
    await screen.findByRole('option', { name: 'Alpha' });
    fireEvent.keyDown(input, { key: 'ArrowDown' });
    fireEvent.keyDown(input, { key: 'ArrowDown' }); // Beta
    fireEvent.change(input, { target: { value: 'a' } }); // triggers another fetch
    await settleDebounce();

    fireEvent.keyDown(input, { key: 'Enter' });
    expect(onSelect).toHaveBeenCalledWith(expect.objectContaining({ id: '2' }));
  });

  // Responses come back in whatever order the network decides. The list must
  // show the answer to the query the user is looking at, not whichever request
  // happened to finish last.
  it('ignores a stale response that lands after a newer one', async () => {
    let call = 0;
    setup({
      allowEmptyQueryFetch: true,
      fetchOptions: async () => {
        call += 1;
        if (call === 1) {
          await new Promise((r) => setTimeout(r, 300));
          return [{ id: '9', label: 'Stale' }];
        }
        return [{ id: '8', label: 'Fresh' }];
      },
    });
    const input = screen.getByRole('combobox', { name: 'picker' });
    input.focus();
    // Long enough for the first fetch to be in flight, short enough that it has
    // not answered yet.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 250));
    });
    fireEvent.change(input, { target: { value: 'x' } });
    await settleDebounce();

    expect(screen.getByRole('option', { name: 'Fresh' })).toBeInTheDocument();
    expect(screen.queryByRole('option', { name: 'Stale' })).toBeNull();
  });

  it('Escape closes the open listbox', async () => {
    setup({ allowEmptyQueryFetch: true });
    const input = screen.getByRole('combobox', { name: 'picker' });
    input.focus();
    await screen.findByRole('listbox');
    fireEvent.keyDown(input, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByRole('listbox')).toBeNull());
  });

  it('shows "Type to search" when empty and empty-fetch is disabled', async () => {
    setup();
    screen.getByRole('combobox', { name: 'picker' }).focus();
    expect(await screen.findByText('Type to search')).toBeInTheDocument();
  });

  it('renders no matches when fetch rejects', async () => {
    const onSelect = vi.fn();
    const fetchOptions = vi.fn().mockRejectedValue(new Error('boom'));
    render(
      <I18nextProvider i18n={i18n}>
        <SearchableCombobox valueId="" valueLabel="" placeholder="p" ariaLabel="picker"
          fetchOptions={fetchOptions} onSelect={onSelect} allowEmptyQueryFetch />
      </I18nextProvider>,
    );
    screen.getByRole('combobox', { name: 'picker' }).focus();
    expect(await screen.findByText('No matches')).toBeInTheDocument();
  });
});
