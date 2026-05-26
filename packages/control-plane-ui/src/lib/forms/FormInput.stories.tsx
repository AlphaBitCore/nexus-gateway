import type { Meta, StoryObj } from '@storybook/react-vite';
import { z } from 'zod';
import { useZodForm } from './useZodForm';
import { FormInput } from './FormInput';

const meta: Meta = {
  title: 'Forms/FormInput',
};
export default meta;
type Story = StoryObj;

const schema = z.object({
  name: z.string().min(1, 'Name is required'),
});

function DefaultDemo() {
  const form = useZodForm({ schema, defaultValues: { name: '' } });
  return <FormInput form={form} name="name" label="Name" placeholder="Enter name" />;
}

export const Default: Story = {
  render: () => <DefaultDemo />,
};

function ValidationErrorDemo() {
  const form = useZodForm({ schema, defaultValues: { name: '' } });
  return (
    <form
      onSubmit={form.handleSubmit(() => {
        /* no-op */
      })}
    >
      <FormInput form={form} name="name" label="Name" required />
      <button type="submit" style={{ marginTop: 8 }}>
        Submit to trigger validation
      </button>
    </form>
  );
}

export const ValidationError: Story = {
  render: () => <ValidationErrorDemo />,
};

function RequiredFieldDemo() {
  const form = useZodForm({ schema, defaultValues: { name: '' } });
  return (
    <FormInput
      form={form}
      name="name"
      label="Display Name"
      required
      helpText="This field is required"
      placeholder="Enter display name"
    />
  );
}

export const RequiredField: Story = {
  render: () => <RequiredFieldDemo />,
};
