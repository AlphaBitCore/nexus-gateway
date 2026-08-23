package vendorbill

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	nexushttp "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/http"
)

const (
	anthropicProviderKey = "anthropic"
	anthropicDefaultBase = "https://api.anthropic.com"
	anthropicCostPath    = "/v1/organizations/cost_report"
	anthropicVersion     = "2023-06-01"
	anthropicMaxPages    = 100

	// anthropicAmountIsCents pins the cost_report amount-scale interpretation
	// (spec §2.2 gotcha 2). The field doc says "lowest currency units (e.g.
	// cents)" and spells out the conversion ("123.45" in "USD" represents
	// $1.23); v1 initially read the worked example as dollars instead and
	// pinned this off, pending "a live known-value day".
	//
	// Settled 2026-07-20 against live data: summing the org's cost_report over
	// 2026-06-01..2026-07-19 gives 71,543.56 under the dollars reading and
	// 715.44 under the cents reading, against an operator-confirmed invoice of
	// ~560 USD for the cycle (cumulative crosses 554.05 on 2026-07-15). The
	// dollars reading is off by 100x; cents matches. Daily values under cents
	// (0.06-128 USD/day) are likewise plausible for the account.
	//
	// Keeping the choice in one constant makes a wrong scale a loud test
	// failure, not a silent 100x money error.
	anthropicAmountIsCents = true
)

// anthropicBillSource reads authoritative daily USD from Anthropic's cost_report
// endpoint, grouped by workspace.
//
// Scope: cost_report accepts NO filter parameters (only starting_at / ending_at
// / bucket_width / group_by / limit / page), and exposes no per-API-key cost at
// all — workspace is its finest attribution unit. So narrowing happens
// client-side: when workspaceID is pinned, results for other workspaces are
// dropped before summing. Unpinned, scope is inferred from the workspace ids
// seen, which collapses to "org" on any account with more than one workspace —
// and also on an account with NONE, because cost_report reports the default
// workspace as a null workspace_id.
type anthropicBillSource struct {
	adminKey    string
	workspaceID string
	baseURL     string
	hc          *http.Client
	maxPages    int
}

func newAnthropicBillSource(adminKey, workspaceID, baseURL string, hc *http.Client) *anthropicBillSource {
	if baseURL == "" {
		baseURL = anthropicDefaultBase
	}
	if hc == nil {
		// Vendor billing APIs are a slow, low-volume egress; the shared
		// factory supplies the timeout / transport policy every outbound
		// call in the gateway is held to (CLAUDE.md → Outbound HTTP).
		hc = nexushttp.New(nexushttp.Config{Caller: "hub-vendorbill-anthropic", Timeout: 60 * time.Second})
	}
	return &anthropicBillSource{adminKey: adminKey, workspaceID: workspaceID, baseURL: baseURL, hc: hc, maxPages: anthropicMaxPages}
}

func (s *anthropicBillSource) ProviderKey() string { return anthropicProviderKey }

// BillingHost is the host cost_report is read from — the identity a Provider
// row's baseUrl must match to be billed against this source.
func (s *anthropicBillSource) BillingHost() string { return hostOf(s.baseURL) }

type anthropicCostPage struct {
	Data []struct {
		StartingAt string `json:"starting_at"`
		Results    []struct {
			Amount      string `json:"amount"`
			Currency    string `json:"currency"`
			WorkspaceID string `json:"workspace_id"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}

// applyAmountScale converts a raw numeric amount to USD dollars under a given
// scale interpretation. Split out so both branches are testable without
// shipping the cents interpretation (which anthropicAmountIsCents pins off).
func applyAmountScale(v float64, isCents bool) float64 {
	if isCents {
		return v / 100
	}
	return v
}

// normalizeAmount converts a cost_report amount string to USD dollars, applying
// the pinned scale interpretation (see anthropicAmountIsCents).
func normalizeAmount(raw string) (float64, error) {
	v, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q: %w", raw, err)
	}
	return applyAmountScale(v, anthropicAmountIsCents), nil
}

// parseAnthropicDay accepts either a full RFC3339 timestamp or a bare date.
func parseAnthropicDay(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC().Truncate(24 * time.Hour), nil
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, fmt.Errorf("starting_at %q: %w", s, err)
	}
	return t.UTC().Truncate(24 * time.Hour), nil
}

func (s *anthropicBillSource) FetchDailyBill(ctx context.Context, from, to time.Time) ([]VendorDailyBill, error) {
	start := from.UTC().Truncate(24 * time.Hour)
	end := to.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1) // exclusive upper bound covers `to`

	perDay := map[time.Time]float64{}
	var scopeIDs []string
	var page string
	for i := 0; ; i++ {
		if i >= s.maxPages {
			return nil, fmt.Errorf("anthropic cost_report: exceeded %d pages", s.maxPages)
		}
		q := url.Values{}
		q.Set("starting_at", start.Format(time.RFC3339))
		q.Set("ending_at", end.Format(time.RFC3339))
		q.Add("group_by[]", "workspace_id")
		q.Set("limit", "31")
		if page != "" {
			q.Set("page", page)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+anthropicCostPath+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("x-api-key", s.adminKey)
		req.Header.Set("anthropic-version", anthropicVersion)

		var pg anthropicCostPage
		if err := getJSON(s.hc, req, &pg); err != nil {
			return nil, fmt.Errorf("anthropic cost_report: %w", err)
		}
		for _, b := range pg.Data {
			day, err := parseAnthropicDay(b.StartingAt)
			if err != nil {
				return nil, fmt.Errorf("anthropic cost_report: %w", err)
			}
			// The bucket EXISTING is the vendor's statement about the day, so
			// the key is created before any result is read — and before the
			// workspace filter below can discard every result in it. On a
			// pinned workspace that filter is exactly what an idle day looks
			// like: the org's other workspaces spent money, ours did not, and
			// the day would otherwise vanish as if the vendor had never
			// reported it. Days genuinely absent from the response stay absent.
			if _, seen := perDay[day]; !seen {
				perDay[day] = 0
			}
			for _, r := range b.Results {
				// Client-side narrowing: the endpoint has no filter parameter,
				// so a pinned workspace is enforced here. Skipped before the
				// currency check so an unrelated workspace billed in another
				// currency cannot fail a scoped fetch.
				if s.workspaceID != "" && r.WorkspaceID != s.workspaceID {
					continue
				}
				if r.Currency != "" && !strings.EqualFold(r.Currency, "usd") {
					return nil, fmt.Errorf("anthropic cost_report: non-USD currency %q", r.Currency)
				}
				amt, err := normalizeAmount(r.Amount)
				if err != nil {
					return nil, fmt.Errorf("anthropic cost_report: %w", err)
				}
				perDay[day] += amt
				scopeIDs = append(scopeIDs, r.WorkspaceID)
			}
		}
		if !pg.HasMore || pg.NextPage == "" {
			break
		}
		page = pg.NextPage
	}

	// A pinned workspace means everything summed above belongs to it — the scope
	// is known, not inferred.
	kind, id := scopeWorkspace, s.workspaceID
	if s.workspaceID == "" {
		kind, id = resolveScope(scopeWorkspace, scopeIDs)
	}
	out := make([]VendorDailyBill, 0, len(perDay))
	for day, amt := range perDay {
		out = append(out, VendorDailyBill{Day: day, AmountUSD: amt, ScopeKind: kind, ScopeID: id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	return out, nil
}
