import { useController, type UseFormReturn, type FieldValues, type Path } from 'react-hook-form';
import { FormField, Checkbox } from '@/components/ui';

interface FormCheckboxProps<T extends FieldValues> {
  form: UseFormReturn<T>;
  name: Path<T>;
  label: string;
  helpText?: string;
  tooltip?: React.ReactNode;
  disabled?: boolean;
  className?: string;
}

export function FormCheckbox<T extends FieldValues>({
  form,
  name,
  label,
  helpText,
  tooltip,
  disabled,
  className,
}: FormCheckboxProps<T>) {
  const { field } = useController({ name, control: form.control });

  return (
    <FormField label={label} helpText={helpText} tooltip={tooltip} className={className}>
      <Checkbox
        checked={!!field.value}
        onCheckedChange={field.onChange}
        disabled={disabled}
      />
    </FormField>
  );
}
