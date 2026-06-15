/**
 * Returns the deployment path prefix injected by nginx via <base href>.
 *
 * nginx injects <base href="/nexus/"> when serving behind a sub-path proxy.
 * Direct access at "/" injects <base href="/"> (or nothing in local dev).
 *
 * Result: "/nexus" when proxied, "" when at root, "" in local dev (no <base> tag).
 */
export function deploymentPrefix(): string {
  if (typeof document === 'undefined') return '';
  const base = document.querySelector('base')?.getAttribute('href') ?? '/';
  return base === '/' ? '' : base.replace(/\/$/, '');
}

/**
 * Prepend the deployment prefix to a path.
 * withPrefix('/oauth/authorize') → '/nexus/oauth/authorize' when at /nexus/.
 * withPrefix('/oauth/authorize') → '/oauth/authorize' when at root.
 */
export function withPrefix(path: string): string {
  return deploymentPrefix() + path;
}
