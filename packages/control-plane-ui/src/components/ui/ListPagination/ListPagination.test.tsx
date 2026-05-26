import { describe, it, expect, vi } from 'vitest';
import { screen, fireEvent } from '@testing-library/react';
import { renderWithProviders } from '@/test/test-utils';
import { ListPagination } from './ListPagination';

describe('ListPagination', () => {
  it('shows summary, page count, rows-per-page, and disabled nav on a single page', () => {
    const onOffsetChange = vi.fn();
    const onLimitChange = vi.fn();
    renderWithProviders(
      <ListPagination
        offset={0}
        limit={20}
        total={5}
        onOffsetChange={onOffsetChange}
        onLimitChange={onLimitChange}
      />,
    );
    expect(screen.getByLabelText('Rows per page')).toBeTruthy();
    expect(screen.getByText('1–5 of 5')).toBeTruthy();
    expect(screen.getByText('Page 1 of 1')).toBeTruthy();
    expect((screen.getByRole('button', { name: 'First page' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Previous page' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Next page' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Last page' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('renders range and calls onOffsetChange for First, Previous, Next, and Last', () => {
    const onOffsetChange = vi.fn();
    const onLimitChange = vi.fn();
    renderWithProviders(
      <ListPagination
        offset={20}
        limit={20}
        total={55}
        onOffsetChange={onOffsetChange}
        onLimitChange={onLimitChange}
      />,
    );

    expect(screen.getByText('21–40 of 55')).toBeTruthy();
    expect(screen.getByText('Page 2 of 3')).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'First page' }));
    expect(onOffsetChange).toHaveBeenCalledWith(0);

    fireEvent.click(screen.getByRole('button', { name: 'Previous page' }));
    expect(onOffsetChange).toHaveBeenCalledWith(0);

    fireEvent.click(screen.getByRole('button', { name: 'Next page' }));
    expect(onOffsetChange).toHaveBeenCalledWith(40);

    fireEvent.click(screen.getByRole('button', { name: 'Last page' }));
    expect(onOffsetChange).toHaveBeenCalledWith(40);
  });

  it('disables Previous and First at offset 0; disables Next and Last on last page', () => {
    const onOffsetChange = vi.fn();
    const onLimitChange = vi.fn();
    const { rerender } = renderWithProviders(
      <ListPagination
        offset={0}
        limit={20}
        total={50}
        onOffsetChange={onOffsetChange}
        onLimitChange={onLimitChange}
      />,
    );
    expect((screen.getByRole('button', { name: 'First page' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Previous page' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Next page' }) as HTMLButtonElement).disabled).toBe(false);
    expect((screen.getByRole('button', { name: 'Last page' }) as HTMLButtonElement).disabled).toBe(false);

    rerender(
      <ListPagination
        offset={40}
        limit={20}
        total={50}
        onOffsetChange={onOffsetChange}
        onLimitChange={onLimitChange}
      />,
    );
    expect((screen.getByRole('button', { name: 'Next page' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Last page' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'First page' }) as HTMLButtonElement).disabled).toBe(false);
    expect((screen.getByRole('button', { name: 'Previous page' }) as HTMLButtonElement).disabled).toBe(false);
  });

  it('calls onLimitChange and resets offset when page size changes', () => {
    const onOffsetChange = vi.fn();
    const onLimitChange = vi.fn();
    renderWithProviders(
      <ListPagination
        offset={40}
        limit={20}
        total={100}
        onOffsetChange={onOffsetChange}
        onLimitChange={onLimitChange}
      />,
    );
    fireEvent.change(screen.getByLabelText('Rows per page'), { target: { value: '50' } });
    expect(onLimitChange).toHaveBeenCalledWith(50);
    expect(onOffsetChange).toHaveBeenCalledWith(0);
  });

  it('hides rows-per-page when showLimitSelect is false', () => {
    renderWithProviders(
      <ListPagination
        offset={0}
        limit={20}
        total={40}
        showLimitSelect={false}
        onOffsetChange={vi.fn()}
        onLimitChange={vi.fn()}
      />,
    );
    expect(screen.queryByLabelText('Rows per page')).toBeNull();
    expect(screen.getByRole('navigation', { name: 'Pagination' })).toBeTruthy();
  });

  it('renders nothing when total is 0', () => {
    renderWithProviders(
      <ListPagination
        offset={0}
        limit={20}
        total={0}
        onOffsetChange={vi.fn()}
        onLimitChange={vi.fn()}
      />,
    );
    expect(screen.queryByRole('navigation', { name: 'Pagination' })).toBeNull();
  });
});
