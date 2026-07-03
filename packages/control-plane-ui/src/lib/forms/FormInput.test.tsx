import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useForm } from 'react-hook-form';

vi.mock('react-i18next', () => ({ useTranslation: () => ({ t: (k: string) => k }) }));
// The '@/components/ui' barrel transitively imports '@/i18n' (Header/Sidebar
// language switcher), whose http-backend import has no place in a unit test.
vi.mock('@/i18n', () => ({ SUPPORTED_LANGUAGES: [], LANGUAGE_STORAGE_KEY: 'lang', default: {} }));

import { FormInput } from './FormInput';

function Harness({ onValueBlur }: { onValueBlur?: (v: string) => void }) {
  const form = useForm<{ name: string }>({ defaultValues: { name: '' } });
  return <FormInput form={form} name="name" label="Name" onValueBlur={onValueBlur} />;
}

describe('FormInput onValueBlur', () => {
  it('fires with the field value when the user leaves the field', async () => {
    const onValueBlur = vi.fn();
    render(<Harness onValueBlur={onValueBlur} />);
    const input = screen.getByLabelText(/Name/);
    await userEvent.type(input, 'my-key');
    await userEvent.tab();
    expect(onValueBlur).toHaveBeenCalledWith('my-key');
  });

  it('renders and blurs normally when onValueBlur is not provided', async () => {
    render(<Harness />);
    const input = screen.getByLabelText(/Name/);
    await userEvent.type(input, 'x');
    await userEvent.tab();
    expect(input).toHaveValue('x');
  });
});
