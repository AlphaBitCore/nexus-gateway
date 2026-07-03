/**
 * FormInput — connects React Hook Form to our Input + FormField components.
 *
 * Handles: registration, error display, required asterisk, help text.
 */
import { useController, type UseFormReturn, type FieldValues, type Path } from 'react-hook-form';
import { FormField, Input } from '@/components/ui';

interface FormInputProps<T extends FieldValues> {
  form: UseFormReturn<T>;
  name: Path<T>;
  label: string;
  helpText?: string;
  tooltip?: React.ReactNode;
  required?: boolean;
  type?: string;
  placeholder?: string;
  disabled?: boolean;
  className?: string;
  /**
   * Called with the field's current value after RHF's own blur handling —
   * for advisory async checks (e.g. duplicate-name probes) that should run
   * when the user leaves the field.
   */
  onValueBlur?: (value: string) => void;
}

export function FormInput<T extends FieldValues>({
  form,
  name,
  label,
  helpText,
  tooltip,
  required,
  type,
  placeholder,
  disabled,
  className,
  onValueBlur,
}: FormInputProps<T>) {
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
      <Input
        {...field}
        value={field.value ?? ''}
        onBlur={e => {
          field.onBlur();
          onValueBlur?.(e.target.value);
        }}
        type={type}
        placeholder={placeholder}
        disabled={disabled}
        error={!!fieldState.error}
      />
    </FormField>
  );
}
