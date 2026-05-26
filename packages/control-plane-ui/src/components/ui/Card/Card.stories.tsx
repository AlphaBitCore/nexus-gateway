import type { Meta, StoryObj } from '@storybook/react-vite';
import { Card } from './Card';

const meta: Meta<typeof Card> = {
  title: 'UI/Card',
  component: Card,
  argTypes: {
    padding: { control: 'select', options: ['none', 'sm', 'md', 'lg'] },
  },
};

export default meta;
type Story = StoryObj<typeof Card>;

export const Default: Story = {
  args: { children: 'Card content with default padding.' },
};

export const NoPadding: Story = {
  args: { children: 'No padding — for wrapping tables.', padding: 'none' },
};

export const Large: Story = {
  args: { children: 'Large padding — for prominent sections.', padding: 'lg' },
};
