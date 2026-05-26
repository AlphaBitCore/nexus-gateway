import type { Meta, StoryObj } from '@storybook/react-vite';
import { z } from 'zod';
import { useZodForm } from './useZodForm';
import { FormTextarea } from './FormTextarea';

const meta: Meta = {
  title: 'Forms/FormTextarea',
};
export default meta;
type Story = StoryObj;

const schema = z.object({
  description: z.string().min(10, 'Description must be at least 10 characters'),
});

function DefaultDemo() {
  const form = useZodForm({ schema, defaultValues: { description: '' } });
  return (
    <FormTextarea
      form={form}
      name="description"
      label="Description"
      placeholder="Enter a description..."
      rows={4}
    />
  );
}

export const Default: Story = {
  render: () => <DefaultDemo />,
};

function WithErrorDemo() {
  const form = useZodForm({ schema, defaultValues: { description: '' } });
  return (
    <form
      onSubmit={form.handleSubmit(() => {
        /* no-op */
      })}
    >
      <FormTextarea
        form={form}
        name="description"
        label="Description"
        placeholder="Enter a description..."
        rows={4}
        required
      />
      <button type="submit" style={{ marginTop: 8 }}>
        Submit to trigger validation
      </button>
    </form>
  );
}

export const WithError: Story = {
  render: () => <WithErrorDemo />,
};
