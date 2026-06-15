// @nexus-gateway/ui-shared
//
// Pure presentational React + types + i18n shared between
// @nexus-gateway/control-plane-ui (browser admin app) and
// @nexus-gateway/agent-dashboard (Wails desktop app).
//
// What lives here:
//   - components/  — stateless presentational React components
//   - types/       — TypeScript shapes both apps' fetchers return
//   - i18n/        — common translation keys (cancel, save, …)
//   - styles/      — CSS variable tokens consumed by both apps
//
// What does NOT live here (and never should):
//   - API clients / HTTP fetchers / Wails bindings
//   - React Router or any opinionated routing
//   - App-level providers (auth, theme switcher, …)
//   - Anything that imports from `@nexus-gateway/control-plane-ui`
//     or `@nexus-gateway/agent-dashboard`.
//
// The two apps consume this package as a workspace source dep (no
// build step); each one wraps the components with its own data-
// fetching layer.

export { cn } from './lib/cn';
export * from './shadcn';
export * from './components/Button';
export * from './components/Chip';
export * from './components/ErrorBanner';
export * from './components/HelpIconButton';
export * from './components/IconButton';
export * from './components/LinkButton';
export * from './theme/chartColors';
export * from './theme/ThemeConfig';
export * from './theme/ThemeContext';
export * from './theme/themeLoader';
export * from './theme/completeness';
export * from './types';
