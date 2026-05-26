import type { StorybookConfig } from '@storybook/react-vite';
import path from 'path';
import { fileURLToPath } from 'node:url';

// Storybook 10 evaluates .storybook/main.ts as ESM; __dirname is CJS-only.
const __dirname = path.dirname(fileURLToPath(import.meta.url));

const config: StorybookConfig = {
  stories: ['../src/**/*.stories.@(ts|tsx)'],
  // Storybook 10 removed the addon-essentials aggregate; list the
  // individual addons explicitly. (a11y / docs / onboarding / vitest
  // are already in devDependencies at ^10.)
  addons: [
    '@storybook/addon-a11y',
    '@storybook/addon-docs',
    '@storybook/addon-onboarding',
    '@storybook/addon-vitest',
  ],
  framework: '@storybook/react-vite',
  viteFinal: async (config) => {
    config.resolve = config.resolve ?? {};
    config.resolve.alias = {
      ...config.resolve.alias,
      '@': path.resolve(__dirname, '../src'),
    };
    // Tailwind runs via `postcss.config.mjs` + `@tailwindcss/postcss` (same as `vite dev`).
    return config;
  },
};

export default config;
