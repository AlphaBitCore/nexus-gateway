/**
 * Form infrastructure — React Hook Form + Zod + design system integration.
 *
 * Usage:
 *   import { useZodForm, FormInput, FormSelect, FormSwitch, FormTextarea } from '@/lib/forms';
 *   import { z } from 'zod';
 *
 *   const schema = z.object({ name: z.string().min(1), enabled: z.boolean() });
 *
 *   function MyForm() {
 *     const form = useZodForm({ schema, defaultValues: { name: '', enabled: true } });
 *
 *     return (
 *       <form onSubmit={form.handleSubmit(onSubmit)}>
 *         <FormInput form={form} name="name" label="Name" required />
 *         <FormSwitch form={form} name="enabled" label="Enabled" />
 *         <Button type="submit" loading={form.formState.isSubmitting}>Save</Button>
 *       </form>
 *     );
 *   }
 */

export { useZodForm } from './useZodForm';
export { FormInput } from './FormInput';
export { FormSelect } from './FormSelect';
export { FormSwitch } from './FormSwitch';
export { FormTextarea } from './FormTextarea';
export { FormCheckbox } from './FormCheckbox';
