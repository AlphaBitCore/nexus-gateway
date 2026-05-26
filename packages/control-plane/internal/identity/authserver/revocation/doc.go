// Package revocation owns the revocation store, publisher, and service. The
// package is deliberately isolated from the oauth HTTP layer so the same
// service object can be invoked from admin handlers, the refresh replay hook,
// and the RFC 7009 /oauth/revoke endpoint without layering cycles.
package revocation
