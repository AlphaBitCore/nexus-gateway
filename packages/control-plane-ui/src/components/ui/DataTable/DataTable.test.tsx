import { describe, it, expect, vi } from 'vitest';
import { screen, fireEvent } from '@testing-library/react';
import '@testing-library/jest-dom/vitest';
import { renderWithProviders } from '@/test/test-utils';
import { DataTable, type DataTableColumn } from './DataTable';

interface TestRow {
  id: number;
  name: string;
  status: string;
}

const columns: DataTableColumn<TestRow>[] = [
  { key: 'id', label: 'ID' },
  { key: 'name', label: 'Name' },
  { key: 'status', label: 'Status', sortable: false },
];

const data: TestRow[] = [
  { id: 1, name: 'Alpha', status: 'active' },
  { id: 2, name: 'Beta', status: 'inactive' },
  { id: 3, name: 'Gamma', status: 'active' },
];

describe('DataTable', () => {
  it('renders column headers from column definitions', () => {
    renderWithProviders(<DataTable columns={columns} data={data} hideSearch />);
    expect(screen.getByText('ID')).toBeInTheDocument();
    expect(screen.getByText('Name')).toBeInTheDocument();
    expect(screen.getByText('Status')).toBeInTheDocument();
  });

  it('renders data rows', () => {
    renderWithProviders(<DataTable columns={columns} data={data} hideSearch />);
    expect(screen.getByText('Alpha')).toBeInTheDocument();
    expect(screen.getByText('Beta')).toBeInTheDocument();
    expect(screen.getByText('Gamma')).toBeInTheDocument();
  });

  it('calls onRowClick when a row is clicked', () => {
    const handler = vi.fn();
    renderWithProviders(<DataTable columns={columns} data={data} onRowClick={handler} hideSearch />);
    fireEvent.click(screen.getByText('Alpha'));
    expect(handler).toHaveBeenCalledOnce();
    expect(handler).toHaveBeenCalledWith(data[0]);
  });

  it('calls onRowClick on Enter key', () => {
    const handler = vi.fn();
    renderWithProviders(<DataTable columns={columns} data={data} onRowClick={handler} hideSearch />);
    const row = screen.getByText('Beta').closest('tr')!;
    fireEvent.keyDown(row, { key: 'Enter' });
    expect(handler).toHaveBeenCalledWith(data[1]);
  });

  it('shows loading state with skeleton rows', () => {
    const { container } = renderWithProviders(
      <DataTable columns={columns} data={[]} loading hideSearch />,
    );
    // Skeleton rows should render; headers should still be present
    expect(screen.getByText('ID')).toBeInTheDocument();
    const skeletons = container.querySelectorAll('[class*="skeleton"]');
    expect(skeletons.length).toBeGreaterThan(0);
  });

  it('shows empty state when data is empty', () => {
    renderWithProviders(
      <DataTable columns={columns} data={[]} emptyMessage="Nothing here" hideSearch />,
    );
    expect(screen.getByText('Nothing here')).toBeInTheDocument();
  });

  it('shows default empty message', () => {
    renderWithProviders(<DataTable columns={columns} data={[]} hideSearch />);
    expect(screen.getByText('No data')).toBeInTheDocument();
  });

  it('sorts when a sortable column header is clicked', () => {
    renderWithProviders(<DataTable columns={columns} data={data} hideSearch />);
    const nameHeader = screen.getByText('Name');

    // Click to sort ascending
    fireEvent.click(nameHeader);
    const cells = screen.getAllByRole('cell');
    // First row name cell (index 1 in each row of 3 cols)
    const nameCells = cells.filter((_, i) => i % 3 === 1);
    expect(nameCells[0]).toHaveTextContent('Alpha');
    expect(nameCells[1]).toHaveTextContent('Beta');
    expect(nameCells[2]).toHaveTextContent('Gamma');

    // Click again to sort descending
    fireEvent.click(nameHeader);
    const cellsDesc = screen.getAllByRole('cell');
    const nameCellsDesc = cellsDesc.filter((_, i) => i % 3 === 1);
    expect(nameCellsDesc[0]).toHaveTextContent('Gamma');
    expect(nameCellsDesc[1]).toHaveTextContent('Beta');
    expect(nameCellsDesc[2]).toHaveTextContent('Alpha');
  });

  it('sets aria-sort on sortable column headers', () => {
    renderWithProviders(<DataTable columns={columns} data={data} hideSearch />);
    const nameHeader = screen.getByText('Name').closest('th')!;
    expect(nameHeader).toHaveAttribute('aria-sort', 'none');

    fireEvent.click(nameHeader);
    expect(nameHeader).toHaveAttribute('aria-sort', 'ascending');

    fireEvent.click(nameHeader);
    expect(nameHeader).toHaveAttribute('aria-sort', 'descending');
  });

  it('does not set aria-sort on non-sortable columns', () => {
    renderWithProviders(<DataTable columns={columns} data={data} hideSearch />);
    const statusHeader = screen.getByText('Status').closest('th')!;
    expect(statusHeader).not.toHaveAttribute('aria-sort');
  });

  it('applies className to root element', () => {
    const { container } = renderWithProviders(
      <DataTable columns={columns} data={data} className="custom-class" hideSearch />,
    );
    expect(container.firstElementChild).toHaveClass('custom-class');
  });

  it('renders search input when hideSearch is false', () => {
    renderWithProviders(<DataTable columns={columns} data={data} />);
    expect(screen.getByPlaceholderText('Filter...')).toBeInTheDocument();
  });

  it('hides search input when hideSearch is true', () => {
    renderWithProviders(<DataTable columns={columns} data={data} hideSearch />);
    expect(screen.queryByPlaceholderText('Filter...')).not.toBeInTheDocument();
  });

  it('renders pagination controls', () => {
    renderWithProviders(<DataTable columns={columns} data={data} pageSize={2} hideSearch />);
    expect(screen.getByLabelText('Previous page')).toBeInTheDocument();
    expect(screen.getByLabelText('Next page')).toBeInTheDocument();
    expect(screen.getByRole('navigation', { name: 'Pagination' })).toBeInTheDocument();
  });

  it('skips client pagination when serverPaginated even if len > pageSize', () => {
    const many: TestRow[] = Array.from({ length: 30 }, (_, i) => ({
      id: i + 1,
      name: `Row ${i + 1}`,
      status: 'active',
    }));
    renderWithProviders(
      <DataTable columns={columns} data={many} pageSize={10} hideSearch serverPaginated />,
    );
    expect(screen.queryByRole('navigation', { name: 'Pagination' })).not.toBeInTheDocument();
    expect(screen.getByText('Row 30')).toBeInTheDocument();
  });
});
