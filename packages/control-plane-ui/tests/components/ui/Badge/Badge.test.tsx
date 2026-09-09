import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';
import { Badge, statusToVariant } from '../../../../src/components/ui/Badge/Badge';

describe('Badge', () => {
  it('renders children', () => {
    render(<Badge>Active</Badge>);
    expect(screen.getByText('Active')).toBeInTheDocument();
  });

  it('applies variant class', () => {
    const { container } = render(<Badge variant="success">OK</Badge>);
    const el = container.firstElementChild!;
    expect(el.className).toContain('success');
  });
});

describe('statusToVariant', () => {
  it('maps "active" to "success"', () => {
    expect(statusToVariant('active')).toBe('success');
  });

  it('returns "default" for unknown status', () => {
    expect(statusToVariant('something-random')).toBe('default');
  });

  // An API key's four lifecycle states all have to land somewhere sensible.
  // `expired` and `rotating` were absent and fell through to grey, so a
  // revoked-by-expiry key looked no different from an unrecognised value.
  it('knows every API-key lifecycle state', () => {
    expect(statusToVariant('active')).toBe('success');
    expect(statusToVariant('rotating')).toBe('warning');
    expect(statusToVariant('expired')).toBe('danger');
    expect(statusToVariant('unavailable')).toBe('danger');
  });

  // This is a shared primitive on dozens of badge sites, usually handed a
  // field straight off an API response. An unguarded .toLowerCase()
  // throws inside a DataTable cell render, which unmounts the WHOLE
  // page: one field an endpoint stops sending blanks the screen.
  it('degrades to a grey badge instead of throwing when the field is absent', () => {
    expect(() => statusToVariant(undefined)).not.toThrow();
    expect(() => statusToVariant(null)).not.toThrow();
    expect(statusToVariant(undefined)).toBe('default');
    expect(statusToVariant(null)).toBe('default');
  });
});
