import type { Meta, StoryObj } from '@storybook/react-vite';
import { DataTable, type DataTableColumn } from './DataTable';

interface SampleRow {
  id: number;
  name: string;
  status: string;
  requests: number;
  latency: string;
}

const sampleColumns: DataTableColumn<SampleRow>[] = [
  { key: 'id', label: 'ID', sortable: true },
  { key: 'name', label: 'Name', sortable: true },
  { key: 'status', label: 'Status', sortable: true },
  { key: 'requests', label: 'Requests', sortable: true },
  { key: 'latency', label: 'Latency', sortable: false },
];

const sampleData: SampleRow[] = Array.from({ length: 25 }, (_, i) => ({
  id: i + 1,
  name: `Service ${String.fromCharCode(65 + (i % 26))}`,
  status: i % 3 === 0 ? 'Active' : i % 3 === 1 ? 'Inactive' : 'Pending',
  requests: Math.floor(Math.random() * 10000),
  latency: `${(Math.random() * 500).toFixed(0)}ms`,
}));

const meta: Meta<typeof DataTable> = {
  title: 'UI/DataTable',
  component: DataTable,
};
export default meta;

type Story = StoryObj<typeof DataTable<SampleRow>>;

export const Default: Story = {
  args: {
    columns: sampleColumns,
    data: sampleData,
  },
};

export const LoadingSkeleton: Story = {
  args: {
    columns: sampleColumns,
    data: [],
    loading: true,
  },
};

export const EmptyState: Story = {
  args: {
    columns: sampleColumns,
    data: [],
    emptyMessage: 'No services found',
  },
};

export const SortableColumns: Story = {
  args: {
    columns: sampleColumns,
    data: sampleData,
  },
};

export const ClickableRows: Story = {
  args: {
    columns: sampleColumns,
    data: sampleData,
    onRowClick: (row: SampleRow) => alert(`Clicked: ${row.name}`),
  },
};

export const Frameless: Story = {
  args: {
    columns: sampleColumns,
    data: sampleData,
    frameless: true,
  },
};
