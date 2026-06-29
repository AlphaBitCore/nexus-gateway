// Package forwardheader owns the AI Gateway's forward-header allowlist
// configuration: parsing the YAML block, applying the hard denylist,
// validating per-adapter-type keys against the closed Format set, and
// surfacing a precomputed read-only structure that the provider adapters
// and handler consult at request and response time.
//
// The package is deliberately leaf-ish: it knows nothing about the
// providers package (Format strings come in as plain strings; the
// caller maps to/from typed enums). This avoids an import cycle with
// internal/providers, which is the primary consumer.
//
// See docs/developers/specs/e36/e36-s1-forward-header-yaml-request.md and
// docs/developers/specs/e36/e36-s2-forward-header-yaml-response.md for the design.
package forwardheader

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

// Config is the on-disk YAML shape of the `forwardHeaders` block.
//
// A pointer type is used in the parent (config.Config) so an absent
// `forwardHeaders:` block in the operator's YAML is distinguishable
// from an empty one. When the field is nil, callers should fall back
// to [DefaultConfig].
type Config struct {
	Request  Direction `yaml:"request"`
	Response Direction `yaml:"response"`
}

// Direction holds the request- or response-side configuration. The
// shape is identical for both directions; the meaning of each field
// differs by direction (see [Entry]).
type Direction struct {
	// Base is the universal allowlist for this direction, applied to
	// every adapter type.
	//
	// Request side: this list seeds the effective set; per-adapter-type
	// extensions are added on top.
	// Response side: not used for the request-style flat list; instead
	// the embedded Static / PerRequest fields on Direction.PerAdapterType
	// govern. To keep the YAML schema parallel, Direction also carries
	// BaseStatic / BasePerRequest for response use.
	Base []string `yaml:"base,omitempty"`

	// BaseStatic / BasePerRequest are the response-side analogues of
	// Base. They apply to every adapter type as a baseline before
	// per-adapter-type extensions are merged. Omit on the request side.
	//
	// They are nested under `base:` in the YAML (Static / PerRequest
	// keys); see the embedded defaults file for the exact shape.
	BaseStatic     []string `yaml:"-"` // populated post-unmarshal from BaseRaw
	BasePerRequest []string `yaml:"-"` // populated post-unmarshal from BaseRaw

	// BaseRaw captures the full nested base struct on the response
	// side (`base: { static: [...], perRequest: [...] }`). For the
	// request side it stays nil and Base is consulted instead.
	BaseRaw *baseSplit `yaml:"baseRaw,omitempty"`

	// PerAdapterType overrides per Provider.adapter_type slug.
	// Keys must be members of the Format set passed to [Resolve];
	// unknown keys cause a fatal validation error.
	PerAdapterType map[string]Entry `yaml:"perAdapterType,omitempty"`
}

// baseSplit is the response-side nested shape of `base:`.
type baseSplit struct {
	Static     []string `yaml:"static"`
	PerRequest []string `yaml:"perRequest"`
}

// UnmarshalYAML supports two `base:` shapes:
//   - request: a flat list of header names ("base: [accept, ...]").
//   - response: a nested object with static + perRequest keys.
//
// Detected by the YAML node kind so a single Direction struct serves
// both directions without parallel types.
func (d *Direction) UnmarshalYAML(value *yaml.Node) error {
	type rawDir struct {
		Base           yaml.Node        `yaml:"base"`
		PerAdapterType map[string]Entry `yaml:"perAdapterType"`
	}
	var raw rawDir
	if err := value.Decode(&raw); err != nil {
		return err
	}
	d.PerAdapterType = raw.PerAdapterType
	switch raw.Base.Kind {
	case 0: // absent
		return nil
	case yaml.SequenceNode:
		return raw.Base.Decode(&d.Base)
	case yaml.MappingNode:
		var split baseSplit
		if err := raw.Base.Decode(&split); err != nil {
			return fmt.Errorf("base: %w", err)
		}
		d.BaseStatic = split.Static
		d.BasePerRequest = split.PerRequest
		return nil
	default:
		return fmt.Errorf("base: expected sequence or mapping, got %v", raw.Base.Kind)
	}
}

// Entry holds one adapter type's extension to the allowlist for one
// direction.
//
// Request side uses [Headers]; the other two fields stay nil.
// Response side uses [Static] (cacheable) and [PerRequest] (stripped
// on cache hit); [Headers] stays nil.
//
// A header listed in both Static and PerRequest for the same adapter
// type is a config error (see [Resolve]).
type Entry struct {
	Headers    []string `yaml:"headers,omitempty"`
	Static     []string `yaml:"static,omitempty"`
	PerRequest []string `yaml:"perRequest,omitempty"`
}

// Resolved is the immutable runtime structure produced by [Resolve].
// All maps are precomputed at startup; callers query via [Resolved.Request]
// and [Resolved.Response] without locking.
type Resolved struct {
	request  map[string]map[string]struct{} // formatSlug → set of allowed names (lower-cased)
	response map[string]ResolvedResponseSet
	hash     string
}

// ResolvedResponseSet is the response-side resolved sets for one
// adapter type. Static headers always pass through; PerRequest headers
// are stripped when the response is being served from a cache hit.
type ResolvedResponseSet struct {
	Static     map[string]struct{}
	PerRequest map[string]struct{}
}

// Request returns the effective request allowlist for a Format slug.
// The returned map is read-only; callers must not mutate it.
//
// Unknown formats (not registered with [Resolve]) return an empty map
// rather than a nil map, so caller iteration is always safe.
func (r *Resolved) Request(formatSlug string) map[string]struct{} {
	if r == nil {
		return emptySet
	}
	if s, ok := r.request[formatSlug]; ok {
		return s
	}
	return emptySet
}

// Response returns the effective response sets for a Format slug.
func (r *Resolved) Response(formatSlug string) ResolvedResponseSet {
	if r == nil {
		return emptyResponseSet
	}
	if s, ok := r.response[formatSlug]; ok {
		return s
	}
	return emptyResponseSet
}

// Hash returns a short, deterministic hash of the resolved sets.
// Folded into the response cache key (Cache.BuildKey's allowlistVersion)
// as the allowlist's contribution, so an allowlist change invalidates
// cached entries captured under a different filter. Returns the first
// 8 hex characters of SHA-256 over a canonical encoding of the
// resolved structure.
func (r *Resolved) Hash() string {
	if r == nil {
		return ""
	}
	return r.hash
}

var (
	emptySet         = map[string]struct{}{}
	emptyResponseSet = ResolvedResponseSet{Static: emptySet, PerRequest: emptySet}
)

//go:embed defaults.yaml
var defaultsYAML []byte

// DefaultConfig parses the embedded defaults.yaml and returns the
// canonical default Config. Panics on a malformed embedded file —
// that is a programmer error caught at startup.
func DefaultConfig() Config {
	var c Config
	if err := yaml.Unmarshal(defaultsYAML, &c); err != nil {
		panic(fmt.Sprintf("forwardheader: malformed embedded defaults: %v", err))
	}
	return c
}

// canonicalDefaultFormats is the closed set of Provider.adapter_type
// slugs the gateway ships with. Kept here as a defensive fallback for
// [Default] so the package can resolve without an external caller
// passing the list (typically [providers.AllFormats]). Must stay in
// sync with [providers.AllFormats]; the production startup path does
// not rely on this list and the smoke / unit tests guard the parity.
var canonicalDefaultFormats = []string{
	"openai", "deepseek", "glm", "azure-openai", "anthropic",
	"gemini", "minimax", "bedrock", "vertex", "cohere",
	"huggingface", "replicate", "mistral", "xai", "groq",
	"perplexity", "together", "fireworks", "moonshot",
}

var (
	defaultOnce     sync.Once
	defaultResolved *Resolved
)

// activeResolved is the live allowlist snapshot consumed by the
// hot-swap call sites (adapter.effectiveAllowlist, the handler's
// per-response writer). atomic.Pointer keeps the read path lock-free.
//
// SetActive is called once at startup (main.go after Resolve) from the
// yaml-resolved allowlist. The forwardHeaders block is yaml-only — see
// ai-gateway.{dev,prod}.yaml. The previous snapshot stays alive until
// the last in-flight reader drops its reference (Go GC) so an ongoing
// response never observes a half-swapped allowlist.
var activeResolved atomic.Pointer[Resolved]

// SetActive replaces the live allowlist snapshot. Safe to call at
// any time; the swap is atomic and lock-free for readers.
func SetActive(r *Resolved) {
	activeResolved.Store(r)
}

// Active returns the live allowlist snapshot. May be nil before
// SetActive has been called (early startup, tests). Callers that
// require a usable allowlist should fall back to [Default].
func Active() *Resolved {
	return activeResolved.Load()
}

// Default returns the resolved structure built from the embedded
// defaults.yaml. Computed once on first call; safe for concurrent
// use thereafter. Used as the fallback when callers (typically
// tests) do not wire an explicit *Resolved into a [providers.Adapter].
//
// Panics on a malformed embedded file — programmer error.
func Default() *Resolved {
	defaultOnce.Do(func() {
		r, err := Resolve(DefaultConfig(), canonicalDefaultFormats)
		if err != nil {
			panic(fmt.Sprintf("forwardheader: embedded defaults failed Resolve: %v", err))
		}
		defaultResolved = r
	})
	return defaultResolved
}

// Resolve validates cfg against the hard denylist and the closed
// Format set, then builds the precomputed [Resolved] structure.
//
// validFormats is the closed list of Provider.adapter_type slugs
// (typically providers.AllFormats() lowered to strings). Any
// PerAdapterType key outside this set is a fatal error.
//
// On any validation failure, returns a non-nil error naming the
// offending header / key. Callers (cmd/ai-gateway/main.go) treat
// the error as fatal and abort startup.
func Resolve(cfg Config, validFormats []string) (*Resolved, error) {
	known := make(map[string]struct{}, len(validFormats))
	for _, f := range validFormats {
		known[f] = struct{}{}
	}

	// Validate every header name across every list.
	if err := validateDirection("request", &cfg.Request, known, false); err != nil {
		return nil, err
	}
	if err := validateDirection("response", &cfg.Response, known, true); err != nil {
		return nil, err
	}

	// Validate that no header appears in both Static and PerRequest
	// for the same adapter type on the response side.
	if err := validateNoStaticPerRequestOverlap(cfg.Response); err != nil {
		return nil, err
	}

	// Precompute effective sets for every known Format.
	r := &Resolved{
		request:  make(map[string]map[string]struct{}, len(validFormats)),
		response: make(map[string]ResolvedResponseSet, len(validFormats)),
	}
	requestBase := lowerSet(cfg.Request.Base)
	respBaseStatic := lowerSet(cfg.Response.BaseStatic)
	respBasePerRequest := lowerSet(cfg.Response.BasePerRequest)
	for _, f := range validFormats {
		// Request side: base ∪ perAdapterType[f].headers
		reqSet := cloneSet(requestBase)
		if entry, ok := cfg.Request.PerAdapterType[f]; ok {
			for _, h := range entry.Headers {
				reqSet[strings.ToLower(h)] = struct{}{}
			}
		}
		r.request[f] = reqSet

		// Response side: BaseStatic ∪ perAdapterType[f].static, etc.
		respSet := ResolvedResponseSet{
			Static:     cloneSet(respBaseStatic),
			PerRequest: cloneSet(respBasePerRequest),
		}
		if entry, ok := cfg.Response.PerAdapterType[f]; ok {
			for _, h := range entry.Static {
				respSet.Static[strings.ToLower(h)] = struct{}{}
			}
			for _, h := range entry.PerRequest {
				respSet.PerRequest[strings.ToLower(h)] = struct{}{}
			}
		}
		r.response[f] = respSet
	}

	r.hash = computeHash(r)
	return r, nil
}

// validateDirection checks every header name in cfg against the hard
// denylist and confirms every PerAdapterType key is a known Format
// slug.
//
// isResponse selects the response-only denylist additions (set-cookie,
// www-authenticate, etc.).
func validateDirection(label string, dir *Direction, known map[string]struct{}, isResponse bool) error {
	collect := func(name string) error {
		if err := checkAgainstDenylist(label, name, isResponse); err != nil {
			return err
		}
		return nil
	}

	for _, h := range dir.Base {
		if err := collect(h); err != nil {
			return err
		}
	}
	for _, h := range dir.BaseStatic {
		if err := collect(h); err != nil {
			return err
		}
	}
	for _, h := range dir.BasePerRequest {
		if err := collect(h); err != nil {
			return err
		}
	}

	// Sort PerAdapterType keys for deterministic error reporting.
	keys := make([]string, 0, len(dir.PerAdapterType))
	for k := range dir.PerAdapterType {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := known[k]; !ok {
			return fmt.Errorf("forwardHeaders.%s.perAdapterType: unknown adapter_type %q (must be one of the registered Provider.adapter_type values)", label, k)
		}
		entry := dir.PerAdapterType[k]
		for _, h := range entry.Headers {
			if err := collect(h); err != nil {
				return err
			}
		}
		for _, h := range entry.Static {
			if err := collect(h); err != nil {
				return err
			}
		}
		for _, h := range entry.PerRequest {
			if err := collect(h); err != nil {
				return err
			}
		}
	}
	return nil
}

// Hard denylist: headers the validator refuses to accept regardless of
// which list they appear in. The list is case-insensitive; entries
// here are pre-lowered for direct comparison.
var (
	exactDenylist = []string{
		"authorization",
		"cookie",
		"set-cookie",
		"x-api-key",
		"x-goog-api-key",
		"api-key",
		"proxy-authorization",
		"x-real-ip",
		"www-authenticate",
		"strict-transport-security",
		"content-security-policy",
		"x-frame-options",
		"server",
		"via",
		"x-served-by",
		"cf-ray",
		"content-length",
		"transfer-encoding",
		"connection",
		// Accept-Encoding is permanently denied: forwarding it disables
		// Go net/http.Transport's transparent gzip decompression and
		// caused a real Anthropic SSE production incident. See the
		// load-bearing comment on specAdapter.effectiveAllowlist at
		// packages/ai-gateway/internal/providers/dispatch/spec_adapter.go:~75-81.
		"accept-encoding",
	}
	prefixDenylist = []string{
		"x-amz-",
		"x-forwarded-",
		"x-nexus-",
		"access-control-",
	}
)

// checkAgainstDenylist returns a non-nil error when name is on the
// hard denylist. Lower-case comparison; prefixes match the pre-lowered
// header name.
func checkAgainstDenylist(label, name string, _ bool) error {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return fmt.Errorf("forwardHeaders.%s: empty header name", label)
	}
	for _, exact := range exactDenylist {
		if lower == exact {
			return fmt.Errorf("forwardHeaders.%s: header %q is on the hard denylist (credential / framing / opacity)", label, name)
		}
	}
	for _, prefix := range prefixDenylist {
		if strings.HasPrefix(lower, prefix) {
			return fmt.Errorf("forwardHeaders.%s: header %q matches denylist prefix %q (credential / proxy-attribution / Nexus-internal)", label, name, prefix)
		}
	}
	return nil
}

// BucketDroppedHeader returns a low-cardinality label for a dropped
// header name, suitable for the
// `nexus_forward_header_dropped_total{header}` metric.
//
// Cardinality contract:
//   - If lowerName is an exact match for a hard-denylist entry,
//     return that entry verbatim.
//   - If lowerName starts with a denylist prefix, return the prefix
//     plus a "*" sentinel (e.g. "x-amz-*", "x-forwarded-*").
//   - Otherwise return "other". Custom allowlist headers and arbitrary
//     unknown client / upstream headers all bucket here.
//
// The returned label set is closed and small (~24 names + "other"),
// so the metric series count stays bounded regardless of inbound
// traffic.
func BucketDroppedHeader(lowerName string) string {
	for _, exact := range exactDenylist {
		if lowerName == exact {
			return exact
		}
	}
	for _, prefix := range prefixDenylist {
		if strings.HasPrefix(lowerName, prefix) {
			return prefix + "*"
		}
	}
	return "other"
}

// validateNoStaticPerRequestOverlap rejects a config where the same
// header name appears in both Static and PerRequest for any single
// adapter type. Mixing the two is ambiguous: cache replay must
// either replay or strip — not "both at once".
func validateNoStaticPerRequestOverlap(resp Direction) error {
	// Base level
	staticBase := lowerSet(resp.BaseStatic)
	for _, h := range resp.BasePerRequest {
		if _, ok := staticBase[strings.ToLower(h)]; ok {
			return fmt.Errorf("forwardHeaders.response.base: header %q listed in both static and perRequest", h)
		}
	}

	// Per-adapter-type level
	keys := make([]string, 0, len(resp.PerAdapterType))
	for k := range resp.PerAdapterType {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		entry := resp.PerAdapterType[k]
		entryStatic := lowerSet(entry.Static)
		for _, h := range entry.PerRequest {
			if _, ok := entryStatic[strings.ToLower(h)]; ok {
				return fmt.Errorf("forwardHeaders.response.perAdapterType.%s: header %q listed in both static and perRequest", k, h)
			}
		}
	}
	return nil
}

func lowerSet(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		out[s] = struct{}{}
	}
	return out
}

func cloneSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

// computeHash returns the first 8 hex chars of SHA-256 over a
// canonical sorted encoding of the resolved structure. Stable across
// process restarts when the underlying YAML is byte-identical.
func computeHash(r *Resolved) string {
	h := sha256.New()
	formats := make([]string, 0, len(r.request))
	for f := range r.request {
		formats = append(formats, f)
	}
	sort.Strings(formats)
	for _, f := range formats {
		_, _ = fmt.Fprintf(h, "F=%s\n", f)
		_, _ = fmt.Fprintf(h, "  req=%s\n", joinSorted(r.request[f]))
		resp := r.response[f]
		_, _ = fmt.Fprintf(h, "  resp.static=%s\n", joinSorted(resp.Static))
		_, _ = fmt.Fprintf(h, "  resp.perRequest=%s\n", joinSorted(resp.PerRequest))
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:4])
}

func joinSorted(set map[string]struct{}) string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
