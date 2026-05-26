import type { Meta, StoryObj } from '@storybook/react-vite';
import { FormField } from './FormField';
import { Input } from '../Input';

const meta: Meta<typeof FormField> = {
  title: 'UI/FormField',
  component: FormField,
};

export default meta;
type Story = StoryObj<typeof FormField>;

export const Default: Story = {
  render: () => (
    <FormField label="Email">
      <Input placeholder="you@example.com" />
    </FormField>
  ),
};

export const Required: Story = {
  render: () => (
    <FormField label="Name" required>
      <Input placeholder="Required field" />
    </FormField>
  ),
};

export const WithError: Story = {
  render: () => (
    <FormField label="Email" error="Email is required" required>
      <Input placeholder="you@example.com" error />
    </FormField>
  ),
};

export const WithHelpText: Story = {
  render: () => (
    <FormField label="Password" helpText="Must be at least 8 characters">
      <Input type="password" placeholder="Enter password" />
    </FormField>
  ),
};

export const ErrorOverridesHelp: Story = {
  render: () => (
    <FormField label="Username" helpText="3-20 characters" error="Username is taken">
      <Input placeholder="Choose a username" error />
    </FormField>
  ),
};
