/**
 * Setup relay API service — CA cert, MDM profile, PAC file, and onboarding mode.
 *
 * File download functions use the authenticated `api.download` helper so that
 * the Bearer token is sent with the request (a plain anchor-tag navigation
 * would fail with 401 on Bearer-auth APIs).
 */
import { api } from '../../client';

export interface OnboardingPatchResponse {
  thingId: string;
  enabled: boolean;
  pushedAt: string;
}

/**
 * Download the compliance-proxy CA certificate for `thingId` and trigger a
 * browser file-save dialog with the given filename.
 */
export async function downloadCACert(thingId: string): Promise<void> {
  return api.download(
    `/api/admin/setup/proxy/${encodeURIComponent(thingId)}/ca-cert`,
    undefined,
    'nexus-proxy-ca.crt',
  );
}

/**
 * Download the MDM configuration profile (Apple mobileconfig) for `thingId`
 * and trigger a browser file-save dialog.
 */
export async function downloadMDMProfile(thingId: string, organization?: string): Promise<void> {
  const params: Record<string, string> = {};
  if (organization) params['organization'] = organization;
  return api.download(
    `/api/admin/setup/proxy/${encodeURIComponent(thingId)}/mdm-profile`,
    Object.keys(params).length > 0 ? params : undefined,
    'nexus-proxy.mobileconfig',
  );
}

/**
 * Download the PAC (Proxy Auto-Configuration) file for `thingId` and trigger
 * a browser file-save dialog.
 */
export async function downloadPACFile(
  thingId: string,
  params: { proxyHost: string; proxyPort: string; failOpen?: boolean },
): Promise<void> {
  const qs: Record<string, string> = {
    proxyHost: params.proxyHost,
    proxyPort: params.proxyPort,
  };
  if (params.failOpen !== undefined) qs['failOpen'] = params.failOpen ? 'true' : 'false';
  return api.download(
    `/api/admin/setup/proxy/${encodeURIComponent(thingId)}/pac-file`,
    qs,
    'nexus-proxy.pac',
  );
}

/**
 * Toggle onboarding mode on a compliance-proxy node. Returns the updated
 * state and the timestamp at which the config was pushed.
 */
export async function patchOnboarding(
  thingId: string,
  enabled: boolean,
): Promise<OnboardingPatchResponse> {
  return api.patch<OnboardingPatchResponse>(
    `/api/admin/setup/proxy/${encodeURIComponent(thingId)}/onboarding`,
    { enabled },
  );
}
