// Package vendorbill fetches a provider's authoritative daily billing totals
// from the vendor's own cost API, so the vendor-bill reconciliation job can
// diff them against the gateway's estimated spend (metric_rollup_1d
// billed_cost_usd, routed_provider dimension).
//
// v1 implements the two vendors that expose a dollar-denominated cost API at
// provider×day granularity: OpenAI and Anthropic. Providers without such an
// API (Gemini direct / DeepSeek / Moonshot) have no source and are surfaced as
// "not covered" upstream. See
// docs/superpowers/specs/2026-07-19-vendor-bill-reconciliation-design.md.
package vendorbill

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// VendorDailyBill is one provider's authoritative billed USD for a single UTC
// day, already narrowed to the gateway's scope where the vendor API allows.
type VendorDailyBill struct {
	Day       time.Time // UTC midnight of the day this bill covers
	AmountUSD float64   // vendor-reported USD for that day, within scope
	ScopeKind string    // "api_key" | "project" | "workspace" | "org"
	ScopeID   string    // concrete scope id when narrowed; empty for "org"
}

// VendorBillSource pulls authoritative daily billing totals from one vendor's
// cost API. Implementations own their vendor's endpoint, auth, response shape,
// scope detection, and amount-scale normalization.
type VendorBillSource interface {
	// ProviderKey matches Provider.adapterType ("openai" / "anthropic").
	ProviderKey() string
	// FetchDailyBill returns one VendorDailyBill per day the vendor reports in
	// [from, to] (UTC days, inclusive). Days the vendor has no data for are
	// omitted — absence means "not finalized", never zero. A non-nil error
	// means the whole fetch failed (transport / auth / decode); the caller
	// records the day(s) as fetch_failed and never fabricates a diff.
	FetchDailyBill(ctx context.Context, from, to time.Time) ([]VendorDailyBill, error)
}

const (
	scopeOrg = "org"
	// scopeAPIKey marks a bill the vendor narrowed to one API key — the tightest
	// attribution available, and the only one that equals "what this gateway
	// spent" on an account whose other keys are used elsewhere.
	scopeAPIKey = "api_key"
	// scopeWorkspace marks a bill narrowed to one Anthropic workspace. Anthropic
	// exposes no per-key cost, so workspace is its finest unit.
	scopeWorkspace = "workspace"
)

// usdAmount decodes a vendor money field that may arrive as either a JSON
// number or a decimal string. OpenAI's costs API returns a 20-significant-digit
// STRING ("15.46230655000000000000000000") while its own docs show a bare
// number; a float64-only field decodes the string as a type error and fails the
// entire fetch (observed live 2026-07-20: "cannot unmarshal string into Go
// struct field .data.results.amount.value of type float64", which made OpenAI
// reconciliation impossible while every unit test passed on number fixtures).
// Accepting both shapes means a vendor switching representation degrades to
// nothing at all rather than to a silent zero.
type usdAmount float64

func (a *usdAmount) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "null" || s == "" {
		*a = 0
		return nil
	}
	// Strip the quotes of a string-encoded decimal; a bare number is untouched.
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return fmt.Errorf("amount %q: %w", s, err)
	}
	*a = usdAmount(v)
	return nil
}

// resolveScope collapses the per-result scope ids seen across a window into a
// single (kind, id). Exactly one distinct non-empty id means the query was
// narrowed to that slice (specificKind, e.g. "project"/"workspace"); zero or
// several means the vendor reported org-wide totals.
func resolveScope(specificKind string, ids []string) (kind, id string) {
	seen := map[string]struct{}{}
	for _, s := range ids {
		if s != "" {
			seen[s] = struct{}{}
		}
	}
	if len(seen) == 1 {
		for s := range seen {
			return specificKind, s
		}
	}
	return scopeOrg, ""
}

// getJSON performs the request and decodes a 200 JSON body into out. A non-200
// status (including 401/403 auth failures) is returned as an error so the
// caller maps the day to fetch_failed.
func getJSON(hc *http.Client, req *http.Request, out any) error {
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}
