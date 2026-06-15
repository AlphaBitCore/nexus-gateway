import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { ChatWithNexus } from './ChatWithNexus';
import { runChat } from './streamChat';

// t returns the key so assertions are i18n-init-independent.
vi.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (k: string, vars?: Record<string, string>) => (vars?.cmd ? `${k}:${vars.cmd}` : k) }),
}));
vi.mock('react-router-dom', () => ({
  useNavigate: () => vi.fn(),
}));
vi.mock('./streamChat', () => ({
  runChat: vi.fn(async () => undefined),
  interruptChat: vi.fn(async () => {}),
  newSessionId: () => 'test-sid',
  confirmDecision: vi.fn(async () => ({ ok: true })),
  listSessions: vi.fn(async () => []),
  getSession: vi.fn(async () => null),
  deleteSession: vi.fn(async () => true),
  downloadFile: vi.fn(async () => {}),
  fileIdsIn: () => [],
  listModels: vi.fn(async () => ({ default: '', models: [] })),
}));

function openChat() {
  render(<ChatWithNexus />);
  fireEvent.click(screen.getByRole('button', { name: 'common:assistant.open' }));
  return screen.getByRole('textbox');
}

function type(input: HTMLElement, text: string) {
  fireEvent.change(input, { target: { value: text } });
  fireEvent.submit(input.closest('form') as HTMLFormElement);
}

describe('web chat slash commands', () => {
  beforeEach(() => vi.clearAllMocks());

  it('/help lists the commands locally — nothing reaches the assistant', async () => {
    const input = openChat();
    type(input, '/help');
    await waitFor(() => expect(screen.getByText('common:assistant.slash.help')).toBeInTheDocument());
    expect(runChat).not.toHaveBeenCalled();
  });

  it('an unknown /-prefixed message is NOT intercepted — it reaches the assistant', async () => {
    // An operator message like "/v1/messages returns 404" must never be eaten
    // by the local router; only the four known commands intercept.
    const input = openChat();
    type(input, '/v1/messages returns 404 through the gateway');
    await waitFor(() => expect(runChat).toHaveBeenCalledTimes(1));
  });

  it('/clear starts a fresh conversation', async () => {
    const input = openChat();
    type(input, 'hello there');
    await waitFor(() => expect(runChat).toHaveBeenCalledTimes(1));
    type(screen.getByRole('textbox'), '/clear');
    await waitFor(() => expect(screen.queryByText('hello there')).not.toBeInTheDocument());
  });

  it('plain text is NOT intercepted — it goes to the assistant', async () => {
    const input = openChat();
    type(input, 'how healthy is the fleet');
    await waitFor(() => expect(runChat).toHaveBeenCalledTimes(1));
  });
});
