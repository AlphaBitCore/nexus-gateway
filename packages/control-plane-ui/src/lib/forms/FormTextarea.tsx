import { useController, type UseFormReturn, type FieldValues, type Path } from 'react-hook-form';
import { FormField, Textarea } from '@/components/ui';

interface FormTextareaProps<T extends FieldValues> {
  form: UseFormReturn<T>;
  name: Path<T>;
  label: string;
  helpText?: string;
  tooltip?: React.ReactNode;
  required?: boolean;
  placeholder?: string;
  disabled?: boolean;
  rows?: number;
  className?: string;
}

export function FormTextarea<T extends FieldValues>({
  form,
  name,
  label,
  helpText,
  tooltip,
  required,
  placeholder,
  disabled,
  rows,
  className,
}: FormTextareaProps<T>) {
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
      <Textarea
        {...field}
        value={field.value ?? ''}
        placeholder={placeholder}
        disabled={disabled}
        rows={rows}
        error={!!fieldState.error}
      />
    </FormField>
  );
}
