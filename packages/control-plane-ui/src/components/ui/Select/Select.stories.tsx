import { useState } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { Select } from './Select';

const meta: Meta<typeof Select> = {
  title: 'UI/Select',
  component: Select,
};

export default meta;
type Story = StoryObj<typeof Select>;

const SAMPLE_OPTIONS = [
  { value: 'openai', label: 'OpenAI' },
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'google', label: 'Google AI' },
  { value: 'azure', label: 'Azure OpenAI' },
];

export const Default: Story = {
  render: () => {
    const [value, setValue] = useState('openai');
    return <Select value={value} onValueChange={setValue} options={SAMPLE_OPTIONS} />;
  },
};

export const WithPlaceholder: Story = {
  render: () => {
    const [value, setValue] = useState('');
    return <Select value={value} onValueChange={setValue} options={SAMPLE_OPTIONS} placeholder="Select a provider..." />;
  },
};

export const Error: Story = {
  render: () => {
    const [value, setValue] = useState('');
    return <Select value={value} onValueChange={setValue} options={SAMPLE_OPTIONS} placeholder="Required field" error />;
  },
};

export const Disabled: Story = {
  args: {
    value: 'openai',
    onValueChange: () => {},
    options: SAMPLE_OPTIONS,
    disabled: true,
  },
};
