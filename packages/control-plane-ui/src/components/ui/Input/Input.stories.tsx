import type { Meta, StoryObj } from '@storybook/react-vite';
import { Input } from './Input';

const meta: Meta<typeof Input> = {
  title: 'UI/Input',
  component: Input,
  argTypes: {
    inputSize: { control: 'select', options: ['sm', 'md', 'lg'] },
    error: { control: 'boolean' },
    disabled: { control: 'boolean' },
  },
};

export default meta;
type Story = StoryObj<typeof Input>;

export const Default: Story = {
  args: { placeholder: 'Enter text...' },
};

export const WithValue: Story = {
  args: { value: 'Hello world', readOnly: true },
};

export const Error: Story = {
  args: { placeholder: 'Invalid input', error: true },
};

export const Disabled: Story = {
  args: { placeholder: 'Disabled', disabled: true },
};

export const Small: Story = {
  args: { placeholder: 'Small input', inputSize: 'sm' },
};

export const Large: Story = {
  args: { placeholder: 'Large input', inputSize: 'lg' },
};
