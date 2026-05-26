// Package rulepack implements the Nexus Rule Pack subsystem: YAML-defined
// pattern collections that hooks bind by install ID. A hook's factory
// loads its installs + overrides once at pipeline-build time and reuses
// compiled regexes per request via the shared core.CompilePattern LRU cache.
package rulepack
