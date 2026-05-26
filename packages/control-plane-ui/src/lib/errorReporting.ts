/**
 * Production client error reporting for the dashboard SPA.
 *
 * Set `VITE_CLIENT_ERROR_REPORTING_URL` to an HTTPS endpoint that accepts POST JSON.
 * Payloads avoid cookies and auth headers; they include message/stack, optional React
 * component stack, pathname (not query — may contain secrets), and an optional release id.
 *
 * Uses `navigator.sendBeacon` when possible, then `fetch` with `keepalive: true`.
 */

import type { ErrorInfo } from 'react';

const MAX_TEXT_FIELD_CHARS = 12_000;
const SERVICE_NAME = 'nexus-dashboard';

export type ClientErrorKind = 'react' | 'window' | 'unhandledrejection';

export interface ClientErrorReportInput {
  kind: ClientErrorKind;
  error: Error;
  componentStack?: string;
  filename?: string;
  lineno?: number;
  colno?: number;
}

function truncate(text: string | undefined, max: number): string | undefined {
  if (text === undefined) return undefined;
  if (text.length <= max) return text;
  return `${text.slice(0, max)}…[truncated]`;
}

function reportingUrl(): string | undefined {
  const u = import.meta.env.VITE_CLIENT_ERROR_REPORTING_URL?.trim();
  return u || undefined;
}

/** Exported for unit tests — stable JSON shape for ingest pipelines. */
export function buildClientErrorReportBody(input: ClientErrorReportInput): Record<string, unknown> {
  const pathname =
    typeof window !== 'undefined' && window.location?.pathname ? window.location.pathname : undefined;
  return {
    service: SERVICE_NAME,
    kind: input.kind,
    message: input.error.message,
    stack: truncate(input.error.stack, MAX_TEXT_FIELD_CHARS),
    componentStack: truncate(input.componentStack, MAX_TEXT_FIELD_CHARS),
    filename: input.filename,
    lineno: input.lineno,
    colno: input.colno,
    pathname,
    release: import.meta.env.VITE_APP_RELEASE?.trim() || undefined,
    ts: new Date().toISOString(),
  };
}

function postReport(url: string, body: Record<string, unknown>): void {
  const json = JSON.stringify(body);
  try {
    if (typeof navigator !== 'undefined' && typeof navigator.sendBeacon === 'function') {
      const blob = new Blob([json], { type: 'application/json' });
      if (navigator.sendBeacon(url, blob)) return;
    }
  } catch {
    /* fall through to fetch */
  }
  try {
    void fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: json,
      keepalive: true,
      mode: 'cors',
      credentials: 'omit',
    });
  } catch {
    /* swallow — reporting must never throw */
  }
}

/**
 * Send a client error report when `VITE_CLIENT_ERROR_REPORTING_URL` is set.
 * No-op when unset. Safe to call from error handlers; does not throw.
 */
export function reportClientError(input: ClientErrorReportInput): void {
  const url = reportingUrl();
  if (!url) return;
  postReport(url, buildClientErrorReportBody(input));
}

export function reportReactError(error: Error, info: ErrorInfo): void {
  reportClientError({
    kind: 'react',
    error,
    componentStack: info.componentStack ?? undefined,
  });
}

/**
 * Register `error` and `unhandledrejection` listeners when a reporting URL is configured.
 * Idempotent per page load.
 */
export function initGlobalErrorReporting(): void {
  if (typeof window === 'undefined') return;
  if (!reportingUrl()) return;

  const w = window as Window & { __nexusGlobalErrorReporting?: boolean };
  if (w.__nexusGlobalErrorReporting) return;
  w.__nexusGlobalErrorReporting = true;

  window.addEventListener(
    'error',
    (event) => {
      if (event.defaultPrevented) return;
      const err =
        event.error instanceof Error
          ? event.error
          : new Error(event.message || 'Script error');
      reportClientError({
        kind: 'window',
        error: err,
        filename: event.filename,
        lineno: event.lineno,
        colno: event.colno,
      });
    },
    true,
  );

  window.addEventListener('unhandledrejection', (event) => {
    const { reason } = event;
    const err =
      reason instanceof Error
        ? reason
        : new Error(typeof reason === 'string' ? reason : 'Unhandled promise rejection');
    reportClientError({ kind: 'unhandledrejection', error: err });
  });
}
