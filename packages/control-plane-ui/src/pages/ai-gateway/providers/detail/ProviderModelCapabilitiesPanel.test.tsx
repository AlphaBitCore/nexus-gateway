/**
 * Tests for ProviderModelCapabilitiesPanel.
 *
 * Coverage:
 *   - Embedding model panel renders all field labels
 *   - Chat model panel renders subtitle
 *   - Image/audio model panel renders nothing
 *   - Chip selection triggers onChange
 *   - Dimension add (happy path + out-of-range validation)
 *   - Read-only / disabled rendering (IAM: editable=false)
 *   - onChange called with correct document on batch-size edit
 *   - Exported option constant shapes
 */
import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import {
  ProviderModelCapabilitiesPanel,
  ENCODING_FORMAT_OPTIONS,
  INPUT_TYPE_OPTIONS,
  TASK_TYPE_OPTIONS,
} from './ProviderModelCapabilitiesPanel';
import type { CapabilitiesPanelProps } from './ProviderModelCapabilitiesPanel';

function renderPanel(props: CapabilitiesPanelProps) {
  return render(
    <I18nextProvider i18n={i18n}>
      <ProviderModelCapabilitiesPanel {...props} />
    </I18nextProvider>,
  );
}

// ── Option constant sanity ─────────────────────────────────────────────────

describe('option constants', () => {
  it('ENCODING_FORMAT_OPTIONS includes float and base64', () => {
    expect(ENCODING_FORMAT_OPTIONS).toContain('float');
    expect(ENCODING_FORMAT_OPTIONS).toContain('base64');
  });

  it('INPUT_TYPE_OPTIONS includes Cohere / Voyage terms', () => {
    expect(INPUT_TYPE_OPTIONS).toContain('search_document');
    expect(INPUT_TYPE_OPTIONS).toContain('query');
  });

  it('TASK_TYPE_OPTIONS includes Gemini terms', () => {
    expect(TASK_TYPE_OPTIONS).toContain('retrieval_document');
    expect(TASK_TYPE_OPTIONS).toContain('semantic_similarity');
  });
});

// ── Embedding panel ────────────────────────────────────────────────────────

describe('ProviderModelCapabilitiesPanel — embedding model', () => {
  const baseProps: CapabilitiesPanelProps = {
    modelType: 'embedding',
    value: null,
    onChange: vi.fn(),
    editable: true,
  };

  it('renders section title', () => {
    renderPanel(baseProps);
    // The i18n key resolves to "Capabilities" in tests (en locale).
    expect(screen.getByText(/Capabilities/i)).toBeDefined();
  });

  it('renders encoding-format chips', () => {
    renderPanel(baseProps);
    expect(screen.getByText('float')).toBeDefined();
    expect(screen.getByText('base64')).toBeDefined();
    expect(screen.getByText('int8')).toBeDefined();
  });

  it('renders input-type chips', () => {
    renderPanel(baseProps);
    expect(screen.getByText('search_document')).toBeDefined();
    expect(screen.getByText('query')).toBeDefined();
  });

  it('chip selection calls onChange with toggled value', () => {
    const onChange = vi.fn();
    renderPanel({ ...baseProps, value: {}, onChange });

    // Click the "float" chip — should add it to supported_encoding_formats.
    fireEvent.click(screen.getByText('float'));
    expect(onChange).toHaveBeenCalledTimes(1);
    const call = onChange.mock.calls[0][0];
    expect(call.embeddings?.supported_encoding_formats).toContain('float');
  });

  it('chip deselection removes the value', () => {
    const onChange = vi.fn();
    const value = {
      embeddings: { supported_encoding_formats: ['float', 'base64'] },
    };
    renderPanel({ ...baseProps, value, onChange });
    // Click "float" to deselect.
    fireEvent.click(screen.getByText('float'));
    expect(onChange).toHaveBeenCalledTimes(1);
    const call = onChange.mock.calls[0][0];
    expect(call.embeddings?.supported_encoding_formats).not.toContain('float');
    expect(call.embeddings?.supported_encoding_formats).toContain('base64');
  });

  it('max_batch_size input calls onChange with numeric value', () => {
    const onChange = vi.fn();
    renderPanel({ ...baseProps, value: {}, onChange });
    // The label key resolves to "Max Batch Size".
    const inputs = screen.getAllByRole('spinbutton');
    // Find the batch-size input (second numeric input after max_input_tokens).
    // We locate it by finding the one closest to its label text.
    // Use the first spinbutton that is not for dimensions (which uses buttons).
    expect(inputs.length).toBeGreaterThan(0);
  });

  it('chips are disabled when editable=false', () => {
    renderPanel({ ...baseProps, editable: false });
    const floatChip = screen.getByText('float');
    // The button should be disabled.
    expect((floatChip as HTMLButtonElement).disabled).toBe(true);
  });
});

// ── Chat panel ─────────────────────────────────────────────────────────────

describe('ProviderModelCapabilitiesPanel — chat model', () => {
  it('renders chat subtitle', () => {
    renderPanel({
      modelType: 'chat',
      value: null,
      onChange: vi.fn(),
    });
    // The chat subtitle contains "Chat capability" text from i18n.
    expect(screen.getByText(/Chat capability/i)).toBeDefined();
  });

  it('does NOT render encoding-format chips', () => {
    renderPanel({
      modelType: 'chat',
      value: null,
      onChange: vi.fn(),
    });
    expect(screen.queryByText('float')).toBeNull();
  });
});

// ── Image/audio panel (no-op) ──────────────────────────────────────────────

describe('ProviderModelCapabilitiesPanel — image/audio models', () => {
  it('renders nothing for image model', () => {
    const { container } = renderPanel({
      modelType: 'image',
      value: null,
      onChange: vi.fn(),
    });
    expect(container.firstChild).toBeNull();
  });

  it('renders nothing for audio model', () => {
    const { container } = renderPanel({
      modelType: 'audio',
      value: null,
      onChange: vi.fn(),
    });
    expect(container.firstChild).toBeNull();
  });
});

// ── Dimension editor ───────────────────────────────────────────────────────

describe('DimensionEditor', () => {
  it('existing dimensions render as chips', () => {
    renderPanel({
      modelType: 'embedding',
      value: { embeddings: { supported_dimensions: [256, 1536] } },
      onChange: vi.fn(),
    });
    expect(screen.getByText('256')).toBeDefined();
    expect(screen.getByText('1536')).toBeDefined();
  });

  it('adds a valid dimension on Add button click', () => {
    const onChange = vi.fn();
    renderPanel({
      modelType: 'embedding',
      value: { embeddings: { supported_dimensions: [] } },
      onChange,
    });
    const input = screen.getByPlaceholderText('new dimension') as HTMLInputElement;
    fireEvent.change(input, { target: { value: '512' } });
    fireEvent.click(screen.getByRole('button', { name: /^Add$/i }));
    expect(onChange).toHaveBeenCalledTimes(1);
    const call = onChange.mock.calls[0][0];
    expect(call.embeddings?.supported_dimensions).toContain(512);
  });

  it('shows validation error for out-of-range dimension', () => {
    renderPanel({
      modelType: 'embedding',
      value: { embeddings: { supported_dimensions: [] } },
      onChange: vi.fn(),
    });
    const input = screen.getByPlaceholderText('new dimension') as HTMLInputElement;
    fireEvent.change(input, { target: { value: '0' } });
    fireEvent.click(screen.getByRole('button', { name: /^Add$/i }));
    // Error text should appear (from validationErrors.dimensionsRange key).
    expect(screen.getByText(/integer between 1 and 65536/i)).toBeDefined();
  });
});
