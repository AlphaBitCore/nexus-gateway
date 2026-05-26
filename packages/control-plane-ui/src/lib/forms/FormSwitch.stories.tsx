import type { Meta, StoryObj } from '@storybook/react-vite';
import { z } from 'zod';
import { useZodForm } from './useZodForm';
import { FormSwitch } from './FormSwitch';

const meta: Meta = {
  title: 'Forms/FormSwitch',
};
export default meta;
type Story = StoryObj;

const schema = z.object({
  enabled: z.boolean(),
});

function UncheckedDemo() {
  const form = useZodForm({ schema, defaultValues: { enabled: false } });
  return <FormSwitch form={form} name="enabled" label="Enable caching" />;
}

export const Unchecked: Story = {
  render: () => <UncheckedDemo />,
};

function CheckedDemo() {
  const form = useZodForm({ schema, defaultValues: { enabled: true } });
  return <FormSwitch form={form} name="enabled" label="Enable caching" />;
}

export const Checked: Story = {
  render: () => <CheckedDemo />,
};
