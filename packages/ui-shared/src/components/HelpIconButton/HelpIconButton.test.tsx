import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import '@testing-library/jest-dom/vitest';
import { HelpIconButton } from './HelpIconButton';

describe('HelpIconButton', () => {
  it('renders a "?" with the aria-label announcing what it explains', () => {
    render(<HelpIconButton aria-label="What is a retry policy?" />);
    const btn = screen.getByRole('button', { name: 'What is a retry policy?' });
    expect(btn).toBeInTheDocument();
    expect(btn).toHaveTextContent('?');
  });

  it('fires onClick (used to open inline help)', () => {
    const handler = vi.fn();
    render(<HelpIconButton aria-label="Help" onClick={handler} />);
    fireEvent.click(screen.getByRole('button', { name: 'Help' }));
    expect(handler).toHaveBeenCalledOnce();
  });
});
