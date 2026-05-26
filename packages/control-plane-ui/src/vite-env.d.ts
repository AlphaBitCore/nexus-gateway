/// <reference types="vite/client" />

interface ImportMetaEnv {
  /**
   * Optional absolute URL (HTTPS) to POST JSON client error reports.
   * When unset, no reports are sent (development still logs to the console where applicable).
   */
  readonly VITE_CLIENT_ERROR_REPORTING_URL?: string;
  /** Optional release or build id included in reports (e.g. git SHA, CI build number). */
  readonly VITE_APP_RELEASE?: string;
}

interface ImportMeta {
  readonly env: ImportMetaEnv;
}
