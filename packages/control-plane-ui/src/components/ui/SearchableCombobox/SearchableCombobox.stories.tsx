import { useState } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { SearchableCombobox, type ComboboxOption } from './SearchableCombobox';

const allOptions: ComboboxOption[] = [
  { id: '1', label: 'GPT-4o' },
  { id: '2', label: 'GPT-4o Mini' },
  { id: '3', label: 'Claude 3.5 Sonnet' },
  { id: '4', label: 'Claude 3 Opus' },
  { id: '5', label: 'Gemini 1.5 Pro' },
  { id: '6', label: 'Llama 3.1 70B' },
];

const fetchOptions = async (query: string): Promise<ComboboxOption[]> => {
  await new Promise((r) => setTimeout(r, 200));
  if (!query) return allOptions;
  const q = query.toLowerCase();
  return allOptions.filter((o) => o.label.toLowerCase().includes(q));
};

const meta: Meta<typeof SearchableCombobox> = {
  title: 'UI/SearchableCombobox',
  component: SearchableCombobox,
};
export default meta;
type Story = StoryObj<typeof SearchableCombobox>;

function DefaultDemo() {
  const [selected, setSelected] = useState<ComboboxOption | null>(null);
  return (
    <SearchableCombobox
      valueId={selected?.id ?? ''}
      valueLabel={selected?.label ?? ''}
      placeholder="Select a model..."
      ariaLabel="Model selector"
      fetchOptions={fetchOptions}
      onSelect={setSelected}
      allowEmptyQueryFetch
    />
  );
}

export const Default: Story = {
  render: () => <DefaultDemo />,
};

function PreSelectedDemo() {
  const [selected, setSelected] = useState<ComboboxOption | null>(allOptions[2]);
  return (
    <SearchableCombobox
      valueId={selected?.id ?? ''}
      valueLabel={selected?.label ?? ''}
      placeholder="Select a model..."
      ariaLabel="Model selector"
      fetchOptions={fetchOptions}
      onSelect={setSelected}
      allowEmptyQueryFetch
    />
  );
}

export const WithPreSelectedValue: Story = {
  render: () => <PreSelectedDemo />,
};

function LoadingDemo() {
  const [selected, setSelected] = useState<ComboboxOption | null>(null);
  const slowFetch = async (query: string): Promise<ComboboxOption[]> => {
    await new Promise((r) => setTimeout(r, 3000));
    const q = query.toLowerCase();
    return allOptions.filter((o) => o.label.toLowerCase().includes(q));
  };
  return (
    <SearchableCombobox
      valueId={selected?.id ?? ''}
      valueLabel={selected?.label ?? ''}
      placeholder="Type to search (slow response)..."
      ariaLabel="Model selector"
      fetchOptions={slowFetch}
      onSelect={setSelected}
      allowEmptyQueryFetch
    />
  );
}

export const LoadingState: Story = {
  render: () => <LoadingDemo />,
};

function KeyboardNavDemo() {
  const [selected, setSelected] = useState<ComboboxOption | null>(null);
  return (
    <div>
      <p style={{ marginBottom: 8, fontSize: 14, color: '#666' }}>
        Focus the input and use Arrow Down/Up, Home, End, Enter, and Escape.
      </p>
      <SearchableCombobox
        valueId={selected?.id ?? ''}
        valueLabel={selected?.label ?? ''}
        placeholder="Try keyboard navigation..."
        ariaLabel="Model selector"
        fetchOptions={fetchOptions}
        onSelect={setSelected}
        allowEmptyQueryFetch
      />
    </div>
  );
}

export const KeyboardNavigationDemo: Story = {
  render: () => <KeyboardNavDemo />,
};
