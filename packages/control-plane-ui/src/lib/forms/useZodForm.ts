/**
 * Typed form hook — wraps React Hook Form with Zod validation.
 */
import { useForm, type UseFormProps, type FieldValues } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import type { ZodType } from 'zod';

export interface UseZodFormProps<T extends FieldValues> extends Omit<UseFormProps<T>, 'resolver'> {
  schema: ZodType<T>;
}

export function useZodForm<T extends FieldValues>({
  schema,
  ...props
}: UseZodFormProps<T>) {
  return useForm<T>({
     
    resolver: zodResolver(schema as any) as any,
    mode: 'onBlur',
    reValidateMode: 'onChange',
    ...props,
  });
}
