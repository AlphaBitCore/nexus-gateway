import { useController, type UseFormReturn, type FieldValues, type Path } from 'react-hook-form';
import { FormField, Select } from '@/components/ui';

interface FormSelectProps<T extends FieldValues> {
  form: UseFormReturn<T>;
  name: Path<T>;
  label: string;
  options: Array<{ value: string; label: string; disabled?: boolean }>;
  helpText?: string;
  tooltip?: React.ReactNode;
  required?: boolean;
  placeholder?: string;
  disabled?: boolean;
  className?: string;
}

export function FormSelect<T extends FieldValues>({
  form,
  name,
  label,
  options,
  helpText,
  tooltip,
  required,
  placeholder,
  disabled,
  className,
}: FormSelectProps<T>) {
  const { field, fieldState } = useController({ name, control: form.control });

  return (
    <FormField
      label={label}
      error={fieldState.error?.message}
      helpText={helpText}
      tooltip={tooltip}
      required={required}
      className={className}
    >
      <Select
        value={field.value ?? ''}
        onValueChange={field.onChange}
        options={options}
        placeholder={placeholder}
        disabled={disabled}
        error={!!fieldState.error}
      />
    </FormField>
  );
}
