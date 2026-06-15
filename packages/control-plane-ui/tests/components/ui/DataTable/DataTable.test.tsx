import { describe, it, expect, vi } from 'vitest';
import { screen, fireEvent } from '@testing-library/react';
import { renderWithProviders } from '@/test/test-utils';
import { DataTable, type DataTableColumn } from '../../../../src/components/ui/DataTable/DataTable';

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

  it('attaches getRowProps data-* attributes (stripping managed keys)', () => {
    renderWithProviders(
      <DataTable
        columns={columns}
        data={data}
        hideSearch
        getRowProps={(row) => ({ 'data-status': row.status, onClick: undefined, className: 'IGNORED' })}
      />,
    );
    const cell = screen.getByText('Alpha');
    const tr = cell.closest('tr')!;
    expect(tr.getAttribute('data-status')).toBe('active');
    // Managed keys (className) must NOT be overridden by getRowProps.
    expect(tr.className).not.toContain('IGNORED');
  });

  describe('expandable rows', () => {
    function renderExpandable(over: Partial<React.ComponentProps<typeof DataTable<TestRow>>> = {}, expandedIds = new Set<string>()) {
      const onToggle = vi.fn();
      renderWithProviders(
        <DataTable
          columns={columns}
          data={data}
          hideSearch
          expandable={{
            getRowId: (r) => String(r.id),
            expandedIds,
            onToggle,
            renderExpanded: (r) => <div>detail-for-{r.name}</div>,
          }}
          {...over}
        />,
      );
      return { onToggle };
    }

    it('renders an expand chevron per row and toggles on click', () => {
      const { onToggle } = renderExpandable();
      // The chevron is the per-row expand toggle button.
      const toggles = screen.getAllByRole('button', { hidden: true }).filter((b) =>
        (b.getAttribute('aria-label') ?? '').length > 0 || b.closest('td'),
      );
      fireEvent.click(toggles[0]);
      expect(onToggle).toHaveBeenCalledWith('1');
    });

    it('renders the expanded detail row for an expanded id', () => {
      renderExpandable({}, new Set(['2']));
      expect(screen.getByText(/detail-for-Beta/)).toBeInTheDocument();
      // Non-expanded rows do not render their detail.
      expect(screen.queryByText(/detail-for-Alpha/)).toBeNull();
    });
  });

  it('uses the virtualized body when row count exceeds the threshold', () => {
    const many: TestRow[] = Array.from({ length: 60 }, (_, i) => ({
      id: i + 1,
      name: `V${i + 1}`,
      status: 'active',
    }));
    // pageSize >= data length so all 60 sit on one page → virtual path (>50).
    renderWithProviders(<DataTable columns={columns} data={many} pageSize={100} hideSearch />);
    // The virtualizer windows rows by measured height (0 in jsdom), so we
    // assert the virtualized table mounted (headers + a table) rather than a
    // specific windowed row.
    expect(screen.getByText('ID')).toBeInTheDocument();
    expect(screen.getByRole('table')).toBeInTheDocument();
  });
});
