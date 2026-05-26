import { useState } from 'react';
import type { Meta, StoryObj } from '@storybook/react-vite';
import { ListFilterToolbar } from './ListFilterToolbar';

const meta: Meta<typeof ListFilterToolbar> = {
  title: 'UI/ListFilterToolbar',
  component: ListFilterToolbar,
};
export default meta;
type Story = StoryObj<typeof ListFilterToolbar>;

function WithSearchDemo() {
  const [search, setSearch] = useState('');
  return (
    <ListFilterToolbar
      searchPlaceholder="Search models..."
      searchValue={search}
      onSearchChange={setSearch}
    />
  );
}

export const WithSearch: Story = {
  render: () => <WithSearchDemo />,
};

function WithChildrenDemo() {
  const [search, setSearch] = useState('');
  return (
    <ListFilterToolbar
      searchPlaceholder="Filter routes..."
      searchValue={search}
      onSearchChange={setSearch}
    >
      <select style={{ padding: '6px 8px', borderRadius: 4 }}>
        <option value="">All statuses</option>
        <option value="active">Active</option>
        <option value="inactive">Inactive</option>
      </select>
      <select style={{ padding: '6px 8px', borderRadius: 4 }}>
        <option value="">All providers</option>
        <option value="openai">OpenAI</option>
        <option value="anthropic">Anthropic</option>
      </select>
    </ListFilterToolbar>
  );
}

export const WithChildren: Story = {
  render: () => <WithChildrenDemo />,
};

function WithMetaCountDemo() {
  const [search, setSearch] = useState('');
  return (
    <ListFilterToolbar
      searchPlaceholder="Search keys..."
      searchValue={search}
      onSearchChange={setSearch}
      meta={<span>Showing 42 of 128 virtual keys</span>}
    />
  );
}

export const WithMetaCount: Story = {
  render: () => <WithMetaCountDemo />,
};
