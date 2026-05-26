/**
 * Canonical UI route paths for the Identity Provider feature.
 * Centralised so all internal links + breadcrumbs reference the same
 * paths (nav move from /system/identity-provider to /iam/identity-
 * providers under the IAM nav section).
 */
export const IDP_LIST_ROUTE = '/iam/identity-providers';
export const IDP_NEW_ROUTE = `${IDP_LIST_ROUTE}/new`;
export const idpDetailRoute = (id: string) => `${IDP_LIST_ROUTE}/${id}`;
