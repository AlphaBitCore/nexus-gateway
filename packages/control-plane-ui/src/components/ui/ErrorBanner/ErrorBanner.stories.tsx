import type { Meta, StoryObj } from '@storybook/react-vite';
import { ErrorBanner } from './ErrorBanner';

const meta: Meta<typeof ErrorBanner> = {
  title: 'UI/ErrorBanner',
  component: ErrorBanner,
};
export default meta;
type Story = StoryObj<typeof ErrorBanner>;

export const Default: Story = {
  args: {
    message: 'Failed to load virtual keys.',
  },
};

export const WithRetryButton: Story = {
  args: {
    message: 'Network error while fetching routes.',
    onRetry: () => alert('Retrying...'),
  },
};

export const LongMessage: Story = {
  args: {
    message:
      'An unexpected error occurred while processing your request. The upstream provider returned a 503 Service Unavailable response after the configured timeout of 30 seconds. Please verify the provider endpoint is healthy and try again.',
    detail: 'Request ID: req_abc123def456 | Provider: openai | Model: gpt-4o',
    onRetry: () => alert('Retrying...'),
    onDismiss: () => alert('Dismissed'),
  },
};
