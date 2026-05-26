import { useController, type UseFormReturn, type FieldValues, type Path } from 'react-hook-form';
import { FormField, Switch } from '@/components/ui';

interface FormSwitchProps<T extends FieldValues> {
  form: UseFormReturn<T>;
  name: Path<T>;
  label: string;
  helpText?: string;
  disabled?: boolean;
  className?: string;
}

export function FormSwitch<T extends FieldValues>({
  form,
  name,
  label,
  helpText,
  disabled,
  className,
}: FormSwitchProps<T>) {
  const { field } = useController({ name, control: form.control });

  return (
    <FormField label={label} helpText={helpText} className={className}>
      <Switch
        checked={!!field.value}
        onCheckedChange={field.onChange}
        disabled={disabled}
      />
    </FormField>
  );
}
