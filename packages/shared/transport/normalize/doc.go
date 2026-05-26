// Package normalize provides the canonical payload normalization pipeline
// used by all three Nexus data-plane services. The public API lives in the
// sub-packages:
//
//   - core/   — registry, payload types, direction/role/kind constants
//   - codecs/ — provider-specific normalizer implementations
//   - extract/ — field-extraction helpers
package normalize
