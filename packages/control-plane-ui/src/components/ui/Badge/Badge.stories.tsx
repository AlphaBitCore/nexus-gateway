import type { Meta, StoryObj } from '@storybook/react-vite';
import { Badge } from './Badge';

const meta: Meta<typeof Badge> = {
  title: 'UI/Badge',
  component: Badge,
  argTypes: {
    variant: { control: 'select', options: ['default', 'success', 'warning', 'danger', 'info', 'outline'] },
  },
};

export default meta;
type Story = StoryObj<typeof Badge>;

export const Default: Story = { args: { children: 'Default' } };
export const Success: Story = { args: { children: 'Active', variant: 'success' } };
export const Warning: Story = { args: { children: 'Degraded', variant: 'warning' } };
export const Danger: Story = { args: { children: 'Error', variant: 'danger' } };
export const Info: Story = { args: { children: 'Pending', variant: 'info' } };
export const Outline: Story = { args: { children: 'Custom', variant: 'outline' } };

export const AllVariants: Story = {
  render: () => (
    <div style={{ display: 'flex', gap: '8px', flexWrap: 'wrap' }}>
      <Badge variant="default">Default</Badge>
      <Badge variant="success">Active</Badge>
      <Badge variant="warning">Degraded</Badge>
      <Badge variant="danger">Error</Badge>
      <Badge variant="info">Pending</Badge>
      <Badge variant="outline">Custom</Badge>
    </div>
  ),
};
