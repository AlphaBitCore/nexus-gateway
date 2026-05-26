import type { Meta, StoryObj } from '@storybook/react-vite';
import { z } from 'zod';
import { useZodForm } from './useZodForm';
import { FormSelect } from './FormSelect';

const meta: Meta = {
  title: 'Forms/FormSelect',
};
export default meta;
type Story = StoryObj;

const schema = z.object({
  provider: z.string().min(1, 'Provider is required'),
});

const providerOptions = [
  { value: 'openai', label: 'OpenAI' },
  { value: 'anthropic', label: 'Anthropic' },
  { value: 'google', label: 'Google' },
  { value: 'mistral', label: 'Mistral' },
];

function WithOptionsDemo() {
  const form = useZodForm({ schema, defaultValues: { provider: '' } });
  return (
    <FormSelect
      form={form}
      name="provider"
      label="Provider"
      options={providerOptions}
      placeholder="Select a provider"
    />
  );
}

export const WithOptions: Story = {
  render: () => <WithOptionsDemo />,
};

function ValidationDemo() {
  const form = useZodForm({ schema, defaultValues: { provider: '' } });
  return (
    <form
      onSubmit={form.handleSubmit(() => {
        /* no-op */
      })}
    >
      <FormSelect
        form={form}
        name="provider"
        label="Provider"
        options={providerOptions}
        placeholder="Select a provider"
        required
      />
      <button type="submit" style={{ marginTop: 8 }}>
        Submit to trigger validation
      </button>
    </form>
  );
}

export const Validation: Story = {
  render: () => <ValidationDemo />,
};
