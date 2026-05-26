import type { Preview } from '@storybook/react-vite';

// Align Storybook with runtime app: Tailwind + shadcn tokens + legacy Nexus semantic CSS.
import '../src/styles/tailwind-app.css';
import '@nexus-gateway/ui-shared/styles/global.css';
import '@nexus-gateway/ui-shared/styles/light.css';
import '@nexus-gateway/ui-shared/styles/dark.css';
import '@nexus-gateway/ui-shared/styles/animations.css';
import '@nexus-gateway/ui-shared/styles/utilities.css';

const preview: Preview = {
  parameters: {
    controls: {
      matchers: {
        color: /(background|color)$/i,
        date: /Date$/i,
      },
    },
    backgrounds: {
      default: 'surface',
      values: [
        { name: 'light', value: '#f8fafc' },
        { name: 'dark', value: '#0a0a0a' },
        { name: 'surface', value: '#ffffff' },
      ],
    },
  },
};

export default preview;
