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
	openaiProviderKey = "openai"
	openaiDefaultBase = "https://api.openai.com"
	openaiCostsPath   = "/v1/organization/costs"
	openaiMaxPages    = 100 // safety bound; a 180-day window paginates well under this
)

// openaiBillSource reads authoritative daily USD from OpenAI's Costs endpoint
// (GET /v1/organization/costs).
//
// Scope: an org-level Admin key sees EVERY project, so summing the response
// answers "what did the whole organization spend", not "what did this gateway
// spend" — useless for reconciliation on any account where the gateway is one
// consumer among several. The endpoint accepts an `api_key_ids` filter, so when
// apiKeyID is pinned the vendor itself narrows the bill to the single key the
// gateway authenticates with; scope is then known exactly, not inferred.
// Unpinned, it falls back to inferring scope from the project ids seen.
type openaiBillSource struct {
	adminKey string
	apiKeyID string
	baseURL  string
	hc       *http.Client
	maxPages int
}

func newOpenAIBillSource(adminKey, apiKeyID, baseURL string, hc *http.Client) *openaiBillSource {
	if baseURL == "" {
		baseURL = openaiDefaultBase
	}
	if hc == nil {
		// Vendor billing APIs are a slow, low-volume egress; the shared
		// factory supplies the timeout / transport policy every outbound
		// call in the gateway is held to (CLAUDE.md → Outbound HTTP).
		hc = nexushttp.New(nexushttp.Config{Caller: "hub-vendorbill-openai", Timeout: 60 * time.Second})
	}
	return &openaiBillSource{adminKey: adminKey, apiKeyID: apiKeyID, baseURL: baseURL, hc: hc, maxPages: openaiMaxPages}
}

func (s *openaiBillSource) ProviderKey() string { return openaiProviderKey }

// BillingHost is the host the Costs endpoint is read from — the identity a
// Provider row's baseUrl must match to be billed against this source.
func (s *openaiBillSource) BillingHost() string { return hostOf(s.baseURL) }

type openaiCostsPage struct {
	Data []struct {
		StartTime int64 `json:"start_time"`
		Results   []struct {
			Amount struct {
				Value    usdAmount `json:"value"`
				Currency string    `json:"currency"`
			} `json:"amount"`
			ProjectID string `json:"project_id"`
		} `json:"results"`
	} `json:"data"`
	HasMore  bool   `json:"has_more"`
	NextPage string `json:"next_page"`
}

func (s *openaiBillSource) FetchDailyBill(ctx context.Context, from, to time.Time) ([]VendorDailyBill, error) {
	start := from.UTC().Truncate(24 * time.Hour)
	end := to.UTC().Truncate(24*time.Hour).AddDate(0, 0, 1) // exclusive upper bound covers `to`

	perDay := map[int64]float64{} // bucket start_time (unix) -> summed USD
	var scopeIDs []string
	var page string
	for i := 0; ; i++ {
		if i >= s.maxPages {
			return nil, fmt.Errorf("openai costs: exceeded %d pages", s.maxPages)
		}
		q := url.Values{}
		q.Set("start_time", strconv.FormatInt(start.Unix(), 10))
		q.Set("end_time", strconv.FormatInt(end.Unix(), 10))
		q.Set("bucket_width", "1d")
		q.Add("group_by", "project_id")
		q.Set("limit", "31")
		if s.apiKeyID != "" {
			// Vendor-side narrowing to the gateway's own key. Without this the
			// sum below is the whole organization's bill.
			q.Add("api_key_ids", s.apiKeyID)
		}
		if page != "" {
			q.Set("page", page)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+openaiCostsPath+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+s.adminKey)

		var pg openaiCostsPage
		if err := getJSON(s.hc, req, &pg); err != nil {
			return nil, fmt.Errorf("openai costs: %w", err)
		}
		for _, b := range pg.Data {
			// The bucket EXISTING is the vendor's statement about the day, so
			// the key is created before any result is read. A day the gateway
			// sent no traffic on comes back as a bucket with an empty results
			// array — the vendor saying "you were charged nothing" — and
			// dropping it because nothing summed would report that day exactly
			// like a day the vendor has not finalized. Downstream those two
			// are opposite: one deserves a $0 row, the other must be left for a
			// later run. Days genuinely absent from the response stay absent.
			if _, seen := perDay[b.StartTime]; !seen {
				perDay[b.StartTime] = 0
			}
			for _, r := range b.Results {
				if r.Amount.Currency != "" && !strings.EqualFold(r.Amount.Currency, "usd") {
					return nil, fmt.Errorf("openai costs: non-USD currency %q", r.Amount.Currency)
				}
				perDay[b.StartTime] += float64(r.Amount.Value)
				scopeIDs = append(scopeIDs, r.ProjectID)
			}
		}
		if !pg.HasMore || pg.NextPage == "" {
			break
		}
		page = pg.NextPage
	}

	// A pinned api key means the vendor already filtered to exactly that key —
	// the scope is known, not inferred from however many projects came back.
	kind, id := scopeAPIKey, s.apiKeyID
	if s.apiKeyID == "" {
		kind, id = resolveScope("project", scopeIDs)
	}
	out := make([]VendorDailyBill, 0, len(perDay))
	for ts, amt := range perDay {
		out = append(out, VendorDailyBill{
			Day:       time.Unix(ts, 0).UTC().Truncate(24 * time.Hour),
			AmountUSD: amt,
			ScopeKind: kind,
			ScopeID:   id,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day.Before(out[j].Day) })
	return out, nil
}
