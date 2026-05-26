import type { Meta, StoryObj } from '@storybook/react-vite';
import { Skeleton } from './Skeleton';

const meta: Meta = {
  title: 'UI/Skeleton',
};
export default meta;
type Story = StoryObj;

export const LinePrimitive: Story = {
  render: () => <Skeleton.Line />,
};

export const HeadingPrimitive: Story = {
  render: () => <Skeleton.Heading />,
};

export const BoxPrimitive: Story = {
  render: () => <Skeleton.Box width={200} height={40} />,
};

export const CirclePrimitive: Story = {
  render: () => <Skeleton.Circle size={48} />,
};

export const Card: Story = {
  render: () => <Skeleton.Card lines={4} />,
};

export const Table: Story = {
  render: () => <Skeleton.Table rows={5} cols={4} />,
};

export const MetricsRow: Story = {
  render: () => <Skeleton.MetricsRow count={4} />,
};

export const DashboardSkeleton: Story = {
  render: () => <Skeleton.DashboardSkeleton />,
};

export const ListPageSkeleton: Story = {
  render: () => <Skeleton.ListPageSkeleton />,
};

export const DetailPageSkeleton: Story = {
  render: () => <Skeleton.DetailPageSkeleton />,
};
