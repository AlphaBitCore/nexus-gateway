// For more info, see https://github.com/storybookjs/eslint-plugin-storybook#configuration-flat-config-format
import storybook from "eslint-plugin-storybook";

import path from 'node:path';
import { fileURLToPath } from 'node:url';
import tseslint from '@typescript-eslint/eslint-plugin';
import tsparser from '@typescript-eslint/parser';
import reactHooks from 'eslint-plugin-react-hooks';

const repoRoot = path.dirname(fileURLToPath(import.meta.url));

export default [
  {
    files: ['packages/control-plane-ui/src/**/*.{ts,tsx}'],
    languageOptions: {
      parser: tsparser,
      parserOptions: {
        project: path.join(repoRoot, 'packages/control-plane-ui/tsconfig.json'),
        tsconfigRootDir: repoRoot,
      },
    },
    plugins: { '@typescript-eslint': tseslint, 'react-hooks': reactHooks },
    rules: {
      'no-console': 'warn',
      '@typescript-eslint/no-unused-vars': ['warn', { argsIgnorePattern: '^_' }],
      'react-hooks/exhaustive-deps': 'warn',
      'react-hooks/rules-of-hooks': 'error',
      'no-restricted-imports': ['warn', {
        patterns: [{
          group: ['../../*', '../../../*'],
          message: 'Use @/ path alias instead of deep relative imports (e.g., @/components/ui, @/hooks/useApi).',
        }],
      }],
      // Design-system primitive guard.
      //
      // Raw <button> elements bypass the design system (Button, IconButton,
      // HelpIconButton, LinkButton, Chip in @nexus-gateway/ui-shared). The
      // primitives drive theme tokens, focus rings, hover/active feedback,
      // and accessibility defaults that get re-implemented inconsistently
      // every time we drop a fresh <button> into a page. New <button> use
      // sites should always reach for a primitive first.
      //
      // Escape hatch: a raw <button> that genuinely doesn't fit any
      // primitive (highly custom auth chrome, Radix-required tag, etc.)
      // must opt out explicitly via `data-design-system-escape="<reason>"`.
      // The attribute serves as inline acknowledgement that the deviation
      // is intentional and gives the next reviewer the why.
      //
      // Severity is 'warn' rather than 'error' so the existing legacy
      // sites (~54 today) don't block the build before they've been
      // either migrated or annotated. Promote to 'error' once the
      // baseline is clean.
      'no-restricted-syntax': [
        'warn',
        {
          selector:
            'JSXOpeningElement[name.name="button"]:not(:has(JSXAttribute[name.name="data-design-system-escape"]))',
          message:
            'Raw <button> bypasses the design system. Use <Button>, <IconButton>, <HelpIconButton>, <LinkButton>, or <Chip> from @nexus-gateway/ui-shared. If genuinely necessary, add data-design-system-escape="<short reason>" to opt out.',
        },
      ],
    },
  },
  // ── @nexus-gateway/ui-shared dependency-direction guard ───────────────
  // ui-shared is the lowest leaf in the workspace dependency graph: it
  // must not import from any application package. Catching this at lint
  // time prevents architectural regressions that would otherwise only
  // surface as runtime circular-dep errors deep inside Vite.
  {
    files: ['packages/ui-shared/src/**/*.{ts,tsx}'],
    languageOptions: {
      parser: tsparser,
      parserOptions: {
        project: path.join(repoRoot, 'packages/ui-shared/tsconfig.json'),
        tsconfigRootDir: repoRoot,
      },
    },
    plugins: { '@typescript-eslint': tseslint },
    rules: {
      '@typescript-eslint/no-unused-vars': ['warn', { argsIgnorePattern: '^_' }],
      'no-restricted-imports': ['error', {
        patterns: [
          {
            group: [
              '@nexus-gateway/control-plane-ui',
              '@nexus-gateway/control-plane-ui/*',
              '@nexus-gateway/agent-dashboard',
              '@nexus-gateway/agent-dashboard/*',
            ],
            message:
              'ui-shared must not depend on any application package. Move the symbol you need down into ui-shared, or invert the dependency.',
          },
          {
            group: ['../../*', '../../../*'],
            message:
              'ui-shared keeps its imports inside the package — avoid deep relative paths that reach into other workspaces.',
          },
        ],
      }],
    },
  },
  // Test + storybook files do not need to follow design-system primitive
  // rules — they construct synthetic buttons to exercise harnesses, not
  // to ship UI.
  {
    files: ['**/*.test.{ts,tsx}', '**/*.stories.{ts,tsx}'],
    rules: { 'no-restricted-syntax': 'off' },
  },
  { ignores: ['**/dist/**', '**/node_modules/**', '**/*.js', '**/*.mjs'] },
  ...storybook.configs['flat/recommended'],
];
