import { describe, it, expect, vi, beforeEach } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { renderWithProviders } from '@/test/test-utils';

const {
  listModels,
  createChatCompletion,
  createChatCompletionStream,
  getUsage,
} = vi.hoisted(() => ({
  listModels: vi.fn(),
  createChatCompletion: vi.fn(),
  createChatCompletionStream: vi.fn(),
  getUsage: vi.fn(),
}));

vi.mock('@/api/services/ai-gateway/aiGatewayClientSimulator', async (importOriginal) => {
  // Keep buildOpenAIBody / pathForRequest / validateRequest / RequestFormat
  // type re-exports from the real module so the page can call them directly
  // (no need to mock pure helpers); only the api surface that hits fetch()
  // is replaced with vi.fn().
  const actual =
    (await importOriginal()) as typeof import('@/api/services/ai-gateway/aiGatewayClientSimulator');
  return {
    ...actual,
    aiGatewayClientSimulatorApi: {
      listModels,
      createChatCompletion,
      createChatCompletionStream,
      getUsage,
    },
  };
});

import { AIGatewaySimulatorPage } from './AIGatewaySimulatorPage';

beforeEach(() => {
  vi.clearAllMocks();
  listModels.mockResolvedValue({
    data: [
      { id: 'gpt-4o', name: 'GPT-4o', owned_by: 'openai' },
      { id: 'gpt-4o-mini', name: 'GPT-4o mini', owned_by: 'openai' },
      { id: 'claude-3-7', name: 'Claude 3.7', owned_by: 'anthropic' },
    ],
  });
  createChatCompletion.mockResolvedValue({
    choices: [{ message: { role: 'assistant', content: 'hello from assistant' } }],
    usage: { prompt_tokens: 10, completion_tokens: 20, total_tokens: 30 },
  });
  getUsage.mockResolvedValue({
    usage: { totalRequests: 5, estimatedCostUsd: 1.23 },
  });
});

describe('AIGatewaySimulatorPage', () => {
  // openSettingsAndConnect drives the Settings dialog flow that every
  // test needs: open the dialog, type the VK, hit Load models, wait for
  // the network call, then close the dialog so the underlying toolbar
  // (provider + model dropdowns) and chat composer are interactive.
  async function openSettingsAndConnect() {
    await userEvent.click(screen.getByRole('button', { name: /^settings$/i }));
    await userEvent.type(screen.getByLabelText(/virtual key|vk/i), 'nvk_test');
    await userEvent.click(screen.getByRole('button', { name: /load models/i }));
    await waitFor(() => expect(listModels).toHaveBeenCalledTimes(1));
    await userEvent.click(screen.getByRole('button', { name: /^done$/i }));
  }

  it('loads models and allows provider/model cascade selection', async () => {
    renderWithProviders(<AIGatewaySimulatorPage />);

    await openSettingsAndConnect();

    // Toolbar comboboxes are now [format, provider, model] — format
    // landed first so the operator decides "OpenAI Chat vs Anthropic
    // Messages vs Gemini" before picking a model.
    const comboBoxes = screen.getAllByRole('combobox');
    expect(comboBoxes[0].textContent).toContain('OpenAI Chat');
    expect(comboBoxes[1].textContent).toContain('anthropic');
    expect(comboBoxes[2].textContent).toContain('Claude 3.7');
  });

  it('sends non-stream chat and refreshes usage', async () => {
    renderWithProviders(<AIGatewaySimulatorPage />);

    await openSettingsAndConnect();

    await userEvent.type(screen.getByLabelText(/chat input/i), 'hello');
    await userEvent.click(screen.getByRole('button', { name: /^send$/i }));

    await waitFor(() => {
      expect(createChatCompletion).toHaveBeenCalledTimes(1);
      expect(getUsage).toHaveBeenCalledTimes(1);
    });
    expect(screen.getByText(/hello from assistant/i)).toBeDefined();
  });

  it('sends only current user message in each request', async () => {
    renderWithProviders(<AIGatewaySimulatorPage />);

    await openSettingsAndConnect();

    await userEvent.type(screen.getByLabelText(/chat input/i), 'first message');
    await userEvent.click(screen.getByRole('button', { name: /^send$/i }));
    await waitFor(() => expect(createChatCompletion).toHaveBeenCalledTimes(1));

    await userEvent.type(screen.getByLabelText(/chat input/i), 'second message');
    await userEvent.click(screen.getByRole('button', { name: /^send$/i }));
    await waitFor(() => expect(createChatCompletion).toHaveBeenCalledTimes(2));

    // Gateway base URL is now resolved server-side (CP env), so the
    // page sends an empty targetUrl. The server fills in the default.
    // SendChatArgs is a single struct now (format/model/messages/params)
    // so we match against the object shape; baseUrl stays empty.
    expect(createChatCompletion).toHaveBeenNthCalledWith(
      1,
      expect.objectContaining({
        baseUrl: '',
        vk: 'nvk_test',
        format: 'openai',
        messages: [{ role: 'user', content: 'first message' }],
      }),
    );
    expect(createChatCompletion).toHaveBeenNthCalledWith(
      2,
      expect.objectContaining({
        baseUrl: '',
        vk: 'nvk_test',
        format: 'openai',
        messages: [{ role: 'user', content: 'second message' }],
      }),
    );
  });
});
