import { api } from '../../../client';

/**
 * Externally-reachable URLs of every server Thing currently
 * registered with the Hub.
 *
 * Backed by `GET /api/admin/services/public-urls`, which queries
 * `thing.metadata.staticInfo.publicURL` grouped by `thing_type` and
 * returns one URL per service (most-recently-seen instance wins for
 * fleets that run multiple of a given service).
 *
 * Use this anywhere the UI needs an "environment-aware" URL —
 * agent install instructions, IdP redirect copy, MDM profile
 * downloads, etc. — instead of hardcoding hostnames.
 *
 * Each field is optional: missing means either (a) no Thing of that
 * type has registered yet, or (b) the running instance's yaml left
 * `publicURL` blank.
 */
export interface ServicePublicURLs {
  hub?: string;
  controlPlane?: string;
  aiGateway?: string;
  complianceProxy?: string;
}

export const serviceUrlsApi = {
  publicURLs: () => api.get<ServicePublicURLs>('/api/admin/services/public-urls'),
};
