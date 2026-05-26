import type { Meta, StoryObj } from '@storybook/react-vite';
import { I18nextProvider } from 'react-i18next';
import i18n from '@/i18n';
import { ErrorBoundary } from './ErrorBoundary';

const meta: Meta<typeof ErrorBoundary> = {
  title: 'UI/ErrorBoundary',
  component: ErrorBoundary,
  decorators: [
    (Story) => (
      <I18nextProvider i18n={i18n}>
        <Story />
      </I18nextProvider>
    ),
  ],
};
export default meta;
type Story = StoryObj<typeof ErrorBoundary>;

function ThrowingComponent(): React.JSX.Element {
  throw new Error('Simulated render error for Storybook demo');
}

export const WidgetLevelFallback: Story = {
  args: {
    level: 'widget',
    children: null,
  },
  render: (args) => (
    <ErrorBoundary {...args}>
      <ThrowingComponent />
    </ErrorBoundary>
  ),
};

export const RouteLevelFallback: Story = {
  args: {
    level: 'route',
    children: null,
  },
  render: (args) => (
    <ErrorBoundary {...args}>
      <ThrowingComponent />
    </ErrorBoundary>
  ),
};
