import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { FormField } from '../../../../src/components/ui/FormField/FormField';

describe('FormField', () => {
  it('renders label text', () => {
    render(
      <FormField label="Username">
        <input />
      </FormField>,
    );
    expect(screen.getByText('Username')).toBeInTheDocument();
  });

  it('renders error message with role="alert"', () => {
    render(
      <FormField label="Email" error="Email is required">
        <input />
      </FormField>,
    );
    const errorEl = screen.getByRole('alert');
    expect(errorEl).toHaveTextContent('Email is required');
  });

  it('links label to input via htmlFor', () => {
    render(
      <FormField label="Password">
        <input />
      </FormField>,
    );
    const label = screen.getByText('Password');
    const input = screen.getByRole('textbox');
    expect(label).toHaveAttribute('for', input.id);
  });

  it('sets aria-describedby on child when error is present', () => {
    render(
      <FormField label="Name" error="Name is required">
        <input />
      </FormField>,
    );
    const input = screen.getByRole('textbox');
    const describedBy = input.getAttribute('aria-describedby');
    expect(describedBy).toBeTruthy();
    const errorEl = screen.getByRole('alert');
    expect(describedBy).toBe(errorEl.id);
  });

  it('sets aria-required on child when required', () => {
    render(
      <FormField label="Email" required>
        <input />
      </FormField>,
    );
    const input = screen.getByRole('textbox');
    expect(input).toHaveAttribute('aria-required', 'true');
  });

  it('sets aria-invalid on child when error is present', () => {
    render(
      <FormField label="Email" error="Invalid email">
        <input />
      </FormField>,
    );
    const input = screen.getByRole('textbox');
    expect(input).toHaveAttribute('aria-invalid', 'true');
  });

  it('does not set aria-invalid when no error', () => {
    render(
      <FormField label="Email">
        <input />
      </FormField>,
    );
    const input = screen.getByRole('textbox');
    expect(input).not.toHaveAttribute('aria-invalid');
  });

  it('shows helpText when no error, hides helpText when error is present', () => {
    const { rerender } = render(
      <FormField label="Bio" helpText="Max 200 characters">
        <input />
      </FormField>,
    );
    expect(screen.getByText('Max 200 characters')).toBeInTheDocument();

    rerender(
      <FormField label="Bio" helpText="Max 200 characters" error="Too long">
        <input />
      </FormField>,
    );
    expect(screen.queryByText('Max 200 characters')).not.toBeInTheDocument();
    expect(screen.getByText('Too long')).toBeInTheDocument();
  });
});
