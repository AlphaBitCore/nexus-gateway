import type { Meta, StoryObj } from '@storybook/react-vite';
import { Grid } from './Grid';
import { Card } from '../Card';

const meta: Meta<typeof Grid> = {
  title: 'UI/Layout/Grid',
  component: Grid,
  argTypes: {
    columns: { control: 'select', options: [1, 2, 3, 4, 5, 6] },
    gap: { control: 'select', options: ['xs', 'sm', 'md', 'lg', 'xl'] },
    responsive: { control: 'select', options: ['viewport', 'container'] },
  },
};

export default meta;
type Story = StoryObj<typeof Grid>;

export const ThreeColumns: Story = {
  render: () => (
    <Grid columns={3} gap="md">
      <Card>Card 1</Card>
      <Card>Card 2</Card>
      <Card>Card 3</Card>
    </Grid>
  ),
};

export const FourColumns: Story = {
  render: () => (
    <Grid columns={4} gap="md">
      <Card>Metric 1</Card>
      <Card>Metric 2</Card>
      <Card>Metric 3</Card>
      <Card>Metric 4</Card>
    </Grid>
  ),
};

export const TwoColumns: Story = {
  render: () => (
    <Grid columns={2} gap="lg">
      <Card padding="lg">Chart Panel Left</Card>
      <Card padding="lg">Chart Panel Right</Card>
    </Grid>
  ),
};
