import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent, waitFor, within } from '@testing-library/react';
import { ChatWithNexus } from './ChatWithNexus';
import { runChat, confirmDecision, deleteSession, downloadFile, listModels } from './streamChat';
import type { StreamCallbacks } from './streamChat';

const navigateMock = vi.fn();

// t returns the key so assertions are i18n-init-independent.
vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (k: string) => k }),
}));
vi.mock('react-router-dom', () => ({
  useNavigate: () => navigateMock,
}));
// Stub the stream so the widget test stays a pure UI test.
vi.mock('./streamChat', () => ({
  runChat: vi.fn(async (_sid: string, _msg: string, cb: StreamCallbacks) => {
    cb.onText?.('hello back');
    cb.onDone?.('new-sid');
  }),
  interruptChat: vi.fn(async () => {}),
  newSessionId: () => 'test-sid',
  confirmDecision: vi.fn(async () => ({ ok: true })),
  listSessions: vi.fn(async () => [{ id: 's1', title: 'earlier chat', updatedAt: 't' }]),
  getSession: vi.fn(async () => ({
    id: 's1',
    messages: [
      { role: 'user', text: 'earlier q' },
      { role: 'assistant', text: 'earlier a' },
    ],
  })),
  deleteSession: vi.fn(async () => true),
  downloadFile: vi.fn(async () => true),
  listModels: vi.fn(async () => ({ default: '', models: [] })),
  fileIdsIn: (text: string) => {
    const ids: string[] = [];
    const re = /\/api\/admin\/assistant\/files\/([a-f0-9]+)/g;
    let m: RegExpExecArray | null;
    while ((m = re.exec(text)) !== null) ids.push(m[1]);
    return ids;
  },
  runIdsIn: () => [],
  reviewVersionIdsIn: () => [],
  workflowVersionIdsIn: () => [],
}));

describe('ChatWithNexus', () => {
  it('is closed initially, opens on the floating button, and streams a reply on send', async () => {
    render(<ChatWithNexus />);
    expect(screen.queryByRole('dialog')).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    expect(screen.getByRole('dialog')).toBeTruthy();

    const input = screen.getByRole('textbox');
    fireEvent.change(input, { target: { value: 'is it healthy?' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);

    await waitFor(() => expect(screen.getByText('is it healthy?')).toBeTruthy());
    await waitFor(() => expect(screen.getByText('hello back')).toBeTruthy());
  });

  it('can be closed again', () => {
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    expect(screen.getByRole('dialog')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'common:close' }));
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('routes to the matching page when the agent emits a navigate directive', async () => {
    navigateMock.mockClear();
    vi.mocked(runChat).mockImplementationOnce(async (_sid: string, _m: string, cb: StreamCallbacks) => {
      cb.onNavigate?.({ view: 'cost' });
      cb.onDone?.();
    });
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    const input = screen.getByRole('textbox');
    fireEvent.change(input, { target: { value: 'show me cost' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);

    await waitFor(() => expect(navigateMock).toHaveBeenCalledWith('/analytics'));
    // The popup stays open (floating over the navigated page) so the conversation is
    // not interrupted by a navigation directive.
    expect(screen.getByRole('dialog')).toBeTruthy();
  });

  it('shows a confirm card for a write tool and posts the decision on Allow', async () => {
    vi.mocked(confirmDecision).mockClear();
    vi.mocked(runChat).mockImplementationOnce(async (_sid: string, _m: string, cb: StreamCallbacks) => {
      cb.onConfirm?.({
        callId: 'c1',
        sessionId: 's1',
        tool: 'mitigate_kill_switch',
        input: { engage: true },
        reason: 'engage kill switch',
        prod: false,
      });
    });
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    const input = screen.getByRole('textbox');
    fireEvent.change(input, { target: { value: 'kill it' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);

    // The confirm card renders with the exact tool + structured input.
    await waitFor(() => expect(screen.getByText(/mitigate_kill_switch/)).toBeTruthy());
    expect(screen.getByText(/"engage":true/)).toBeTruthy();

    // Allow → posts the decision (true) for that session/call.
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.allow' }));
    expect(vi.mocked(confirmDecision)).toHaveBeenCalledWith('s1', 'c1', true);
  });

  it('requires a second confirm in production: first Allow keeps the card, second Allow echoes the token', async () => {
    vi.mocked(confirmDecision).mockClear();
    // First Allow returns a backend-issued challenge token (FR-9); the second resolves.
    vi.mocked(confirmDecision)
      .mockResolvedValueOnce({ ok: true, secondConfirmRequired: true, challengeToken: 'tok-123' })
      .mockResolvedValueOnce({ ok: true });
    vi.mocked(runChat).mockImplementationOnce(async (_sid: string, _m: string, cb: StreamCallbacks) => {
      cb.onConfirm?.({
        callId: 'c9',
        sessionId: 's9',
        tool: 'mitigate_passthrough_global',
        input: { engage: true },
        reason: 'engage passthrough',
        prod: true,
      });
    });
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    const input = screen.getByRole('textbox');
    fireEvent.change(input, { target: { value: 'engage' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);

    await waitFor(() => expect(screen.getByText(/mitigate_passthrough_global/)).toBeTruthy());

    // First Allow: posted with no token; card stays open showing the second step.
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.allow' }));
    expect(vi.mocked(confirmDecision)).toHaveBeenNthCalledWith(1, 's9', 'c9', true);
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'common:assistant.confirmExecute' })).toBeTruthy(),
    );
    expect(screen.getByText('common:assistant.confirmSecondStep')).toBeTruthy();

    // Second Allow: echoes the server-issued token to actually execute.
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.confirmExecute' }));
    expect(vi.mocked(confirmDecision)).toHaveBeenNthCalledWith(2, 's9', 'c9', true, 'tok-123');
    // Card closes once the write is resolved.
    await waitFor(() => expect(screen.queryByText(/mitigate_passthrough_global/)).toBeNull());
  });

  it('renders the FR-22 impact preview (summary + irreversible) for a high-blast tool', async () => {
    vi.mocked(runChat).mockImplementationOnce(async (_sid: string, _m: string, cb: StreamCallbacks) => {
      cb.onConfirm?.({
        callId: 'c7',
        sessionId: 's7',
        tool: 'mitigate_vk_revoke',
        input: { vk: 'app-key' },
        reason: 'revoke app-key',
        prod: false,
        preview: {
          action: 'revoke',
          summary: 'Permanently revokes this Virtual Key; apps using it get 401.',
          irreversible: true,
          current: { virtualKey: 'app-key' },
        },
      });
    });
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    const input = screen.getByRole('textbox');
    fireEvent.change(input, { target: { value: 'revoke' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);

    await waitFor(() => expect(screen.getByText('common:assistant.impactTitle')).toBeTruthy());
    expect(screen.getByText(/Permanently revokes this Virtual Key/)).toBeTruthy();
    expect(screen.getByText('common:assistant.impactIrreversible')).toBeTruthy();
  });

  it('shows the impact preview AND the prod two-step on a high-blast prod confirm', async () => {
    vi.mocked(confirmDecision).mockClear();
    vi.mocked(confirmDecision)
      .mockResolvedValueOnce({ ok: true, secondConfirmRequired: true, challengeToken: 'tok-9' })
      .mockResolvedValueOnce({ ok: true });
    vi.mocked(runChat).mockImplementationOnce(async (_sid: string, _m: string, cb: StreamCallbacks) => {
      cb.onConfirm?.({
        callId: 'c5',
        sessionId: 's5',
        tool: 'mitigate_kill_switch',
        input: { engage: true },
        reason: 'engage kill switch',
        prod: true,
        preview: { action: 'engage', summary: 'Halts TLS bumping fleet-wide.', current: { engaged: false } },
      });
    });
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    const input = screen.getByRole('textbox');
    fireEvent.change(input, { target: { value: 'kill it' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);

    // Preview shows alongside the prod card.
    await waitFor(() => expect(screen.getByText('common:assistant.impactTitle')).toBeTruthy());
    expect(screen.getByText(/Halts TLS bumping fleet-wide/)).toBeTruthy();

    // First Allow → prod second step appears, preview still present.
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.allow' }));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: 'common:assistant.confirmExecute' })).toBeTruthy(),
    );
    expect(screen.getByText('common:assistant.impactTitle')).toBeTruthy();

    // Second Allow executes with the token.
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.confirmExecute' }));
    expect(vi.mocked(confirmDecision)).toHaveBeenNthCalledWith(2, 's5', 'c5', true, 'tok-9');
  });

  it('renders an unavailable impact note (fail-open) without blocking Allow', async () => {
    vi.mocked(runChat).mockImplementationOnce(async (_sid: string, _m: string, cb: StreamCallbacks) => {
      cb.onConfirm?.({
        callId: 'c6',
        sessionId: 's6',
        tool: 'mitigate_kill_switch',
        input: { engage: true },
        reason: 'engage',
        prod: false,
        preview: { unavailable: true, note: 'Impact preview could not be computed; proceed with caution.' },
      });
    });
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    const input = screen.getByRole('textbox');
    fireEvent.change(input, { target: { value: 'engage' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);

    await waitFor(() => expect(screen.getByText(/proceed with caution/)).toBeTruthy());
    // Allow is still available (emergency tool not blocked by a degraded read).
    expect(screen.getByRole('button', { name: 'common:assistant.allow' })).toBeTruthy();
  });

  it('production Deny aborts without a second confirm', async () => {
    vi.mocked(confirmDecision).mockClear();
    vi.mocked(confirmDecision).mockResolvedValue({ ok: true });
    vi.mocked(runChat).mockImplementationOnce(async (_sid: string, _m: string, cb: StreamCallbacks) => {
      cb.onConfirm?.({
        callId: 'c8',
        sessionId: 's8',
        tool: 'mitigate_vk_revoke',
        input: { keyId: 'vk-1' },
        reason: 'revoke',
        prod: true,
      });
    });
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    const input = screen.getByRole('textbox');
    fireEvent.change(input, { target: { value: 'revoke' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);

    await waitFor(() => expect(screen.getByText(/mitigate_vk_revoke/)).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.deny' }));
    expect(vi.mocked(confirmDecision)).toHaveBeenCalledWith('s8', 'c8', false);
    await waitFor(() => expect(screen.queryByText(/mitigate_vk_revoke/)).toBeNull());
  });

  it('opens history, loads a past session transcript, and starts a new chat', async () => {
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.history' }));

    await waitFor(() => expect(screen.getByText('earlier chat')).toBeTruthy());
    fireEvent.click(screen.getByText('earlier chat'));

    // The past transcript re-renders both turns.
    await waitFor(() => expect(screen.getByText('earlier q')).toBeTruthy());
    expect(screen.getByText('earlier a')).toBeTruthy();
    // New chat clears the conversation.
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.newChat' }));
    expect(screen.queryByText('earlier q')).toBeNull();
  });

  it('renders a download button for an assistant file reply and triggers the download', async () => {
    vi.mocked(runChat).mockImplementationOnce(async (_sid: string, _m: string, cb: StreamCallbacks) => {
      cb.onText?.('done; download at /api/admin/assistant/files/deadbeef');
      cb.onDone?.('sid');
    });
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    const input = screen.getByRole('textbox');
    fireEvent.change(input, { target: { value: 'make a file' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);

    await waitFor(() => expect(screen.getByRole('button', { name: /assistant.download/ })).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /assistant.download/ }));
    expect(vi.mocked(downloadFile)).toHaveBeenCalledWith('deadbeef');
  });

  it('renders a download button from the structured file event even when the reply omits the URL', async () => {
    vi.mocked(runChat).mockImplementationOnce(async (_sid: string, _m: string, cb: StreamCallbacks) => {
      // The model's text does NOT contain a download URL — the button must come
      // from the structured `file` event alone (the robustness win).
      cb.onText?.('Saved your report.');
      cb.onFile?.({ id: 'cafe1234', downloadPath: '/api/admin/assistant/files/cafe1234' });
      cb.onDone?.('sid');
    });
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    const input = screen.getByRole('textbox');
    fireEvent.change(input, { target: { value: 'make a file' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);

    await waitFor(() => expect(screen.getByRole('button', { name: /assistant.download/ })).toBeTruthy());
    fireEvent.click(screen.getByRole('button', { name: /assistant.download/ }));
    expect(vi.mocked(downloadFile)).toHaveBeenCalledWith('cafe1234');
  });

  it('offers a searchable model picker and sends the chosen model code', async () => {
    vi.mocked(listModels).mockResolvedValueOnce({
      default: 'm1',
      models: [
        { code: 'm1', label: 'Model One', provider: 'OpenAI' },
        { code: 'm2', label: 'Model Two', provider: 'Anthropic' },
      ],
    });
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));

    // The trigger shows the current (default) model's label, not its code.
    const trigger = await screen.findByRole('button', { name: 'common:assistant.model' });
    expect(trigger.textContent).toBe('Model One');

    // Open the popover, type to filter down to the second model, then click it.
    fireEvent.click(trigger);
    const search = await screen.findByPlaceholderText('common:assistant.modelSearch');
    // The popover scrollable list (sibling of the search box's container).
    const popover = search.closest('[class*="p-0"]') as HTMLElement;
    const list = popover.querySelector('[class*="overflow-y-auto"]') as HTMLElement;
    fireEvent.change(search, { target: { value: 'two' } });
    // m1 is filtered OUT of the option list (the trigger still shows it; scope to the list).
    await waitFor(() => expect(within(list).queryByText('Model One')).toBeNull());
    fireEvent.click(within(list).getByText('Model Two'));

    // The trigger now reflects the new selection.
    await waitFor(() => expect(trigger.textContent).toBe('Model Two'));

    const input = screen.getByRole('textbox');
    fireEvent.change(input, { target: { value: 'hi' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);
    // runChat's 5th arg (the model) is the CODE, not the label.
    await waitFor(() => expect(vi.mocked(runChat).mock.calls.at(-1)?.[4]).toBe('m2'));
  });

  it('groups the model picker options by provider', async () => {
    vi.mocked(listModels).mockResolvedValueOnce({
      default: 'm1',
      models: [
        { code: 'm1', label: 'Model One', provider: 'OpenAI' },
        { code: 'm2', label: 'Model Two', provider: 'Anthropic' },
      ],
    });
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    const trigger = await screen.findByRole('button', { name: 'common:assistant.model' });
    fireEvent.click(trigger);
    const search = await screen.findByPlaceholderText('common:assistant.modelSearch');
    const popover = search.closest('[class*="p-0"]') as HTMLElement;
    const list = popover.querySelector('[class*="overflow-y-auto"]') as HTMLElement;
    // Provider headers are rendered for each group, within the option list.
    await waitFor(() => expect(within(list).getByText('OpenAI')).toBeTruthy());
    expect(within(list).getByText('Anthropic')).toBeTruthy();
    // Both models are listed under their groups.
    expect(within(list).getByText('Model One')).toBeTruthy();
    expect(within(list).getByText('Model Two')).toBeTruthy();
  });

  it('expands a tool chip to reveal the tool input AND its response', async () => {
    vi.mocked(runChat).mockImplementationOnce(async (_sid: string, _m: string, cb: StreamCallbacks) => {
      cb.onToolStart?.('resource_search', { query: 'routing rules' });
      cb.onToolEnd?.('resource_search', false, '{"cards":[{"operationId":"rulesList"}]}');
      cb.onText?.('done');
      cb.onDone?.('sid');
    });
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    const input = screen.getByRole('textbox');
    fireEvent.change(input, { target: { value: 'search' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);

    const chip = await screen.findByRole('button', { name: /assistant.toolDetails/ });
    // Collapsed by default; neither request nor response appears before a click.
    expect(screen.queryByText(/routing rules/)).toBeNull();
    expect(screen.queryByText(/rulesList/)).toBeNull();
    fireEvent.click(chip);
    await waitFor(() => expect(screen.getByText(/routing rules/)).toBeTruthy());
    // The tool's result is visible below the request, under the response label.
    expect(screen.getByText(/rulesList/)).toBeTruthy();
    expect(screen.getByText('common:assistant.toolResponse')).toBeTruthy();
  });

  it('a tool with a response but no input is still expandable', async () => {
    vi.mocked(runChat).mockImplementationOnce(async (_sid: string, _m: string, cb: StreamCallbacks) => {
      cb.onToolStart?.('workflow_list', {});
      cb.onToolEnd?.('workflow_list', false, '- sweep (id wf1, user): audits keys');
      cb.onText?.('done');
      cb.onDone?.('sid');
    });
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    const input = screen.getByRole('textbox');
    fireEvent.change(input, { target: { value: 'list' } });
    fireEvent.submit(input.closest('form') as HTMLFormElement);

    const chip = await screen.findByRole('button', { name: /assistant.toolDetails/ });
    fireEvent.click(chip);
    await waitFor(() => expect(screen.getByText(/audits keys/)).toBeTruthy());
  });

  it('toggles maximize/restore, enlarging the panel', () => {
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    const dialog = screen.getByRole('dialog');
    // Default: compact corner card; only a maximize affordance is shown.
    expect(dialog.className).toContain('w-96');
    expect(screen.queryByRole('button', { name: 'common:assistant.restore' })).toBeNull();

    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.maximize' }));
    // Maximized: near-fullscreen width, and the control now offers restore.
    expect(dialog.className).toContain('w-[min(60rem');
    expect(screen.getByRole('button', { name: 'common:assistant.restore' })).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.restore' }));
    expect(dialog.className).toContain('w-96');
  });

  it('deletes a session from the history list', async () => {
    render(<ChatWithNexus />);
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.history' }));
    await waitFor(() => expect(screen.getByText('earlier chat')).toBeTruthy());

    fireEvent.click(screen.getByRole('button', { name: 'common:assistant.deleteSession' }));
    await waitFor(() => expect(vi.mocked(deleteSession)).toHaveBeenCalledWith('s1'));
    await waitFor(() => expect(screen.queryByText('earlier chat')).toBeNull());
  });
});
