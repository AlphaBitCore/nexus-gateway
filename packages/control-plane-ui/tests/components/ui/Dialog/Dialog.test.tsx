import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { Dialog } from '../../../../src/components/ui/Dialog/Dialog';

describe('Dialog', () => {
  it('renders title when open', () => {
    render(
      <Dialog open onOpenChange={vi.fn()} title="My Dialog">
        <p>Body content</p>
      </Dialog>,
    );
    expect(screen.getByText('My Dialog')).toBeInTheDocument();
  });

  it('renders description when provided', () => {
    render(
      <Dialog
        open
        onOpenChange={vi.fn()}
        title="Title"
        description="A helpful description"
      >
        <p>Body</p>
      </Dialog>,
    );
    expect(screen.getByText('A helpful description')).toBeInTheDocument();
  });

  it('renders children content', () => {
    render(
      <Dialog open onOpenChange={vi.fn()} title="Title">
        <p>Inner content here</p>
      </Dialog>,
    );
    expect(screen.getByText('Inner content here')).toBeInTheDocument();
  });
});
