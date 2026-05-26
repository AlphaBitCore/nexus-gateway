import type { Meta, StoryObj } from '@storybook/react-vite';
import { Stack } from './Stack';

const meta: Meta<typeof Stack> = {
  title: 'UI/Layout/Stack',
  component: Stack,
  argTypes: {
    direction: { control: 'select', options: ['vertical', 'horizontal'] },
    gap: { control: 'select', options: ['xs', 'sm', 'md', 'lg', 'xl'] },
    align: { control: 'select', options: ['start', 'center', 'end', 'stretch'] },
    justify: { control: 'select', options: ['start', 'center', 'end', 'between'] },
  },
};

export default meta;
type Story = StoryObj<typeof Stack>;

const Box = ({ label }: { label: string }) => (
  <div style={{ padding: '12px 20px', background: 'var(--color-muted)', borderRadius: 'var(--g-radius-md)', fontSize: '13px' }}>
    {label}
  </div>
);

export const Vertical: Story = {
  render: () => (
    <Stack gap="md">
      <Box label="Item 1" />
      <Box label="Item 2" />
      <Box label="Item 3" />
    </Stack>
  ),
};

export const Horizontal: Story = {
  render: () => (
    <Stack direction="horizontal" gap="md">
      <Box label="Left" />
      <Box label="Center" />
      <Box label="Right" />
    </Stack>
  ),
};

export const SpaceBetween: Story = {
  render: () => (
    <Stack direction="horizontal" justify="between" fullWidth>
      <Box label="Start" />
      <Box label="End" />
    </Stack>
  ),
};
