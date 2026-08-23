// Package vendorbill fetches a provider's authoritative daily billing totals
// from the vendor's own cost API, so the vendor-bill reconciliation job can
// diff them against the gateway's recorded vendor spend (metric_rollup_1d
// vendor_spend_usd, routed_provider dimension).
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
	"net/url"
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
	// FetchDailyBill returns one VendorDailyBill per day the vendor REPORTED a
	// bucket for in [from, to] (UTC days, inclusive).
	//
	// A day the vendor returned a bucket for is always present, including at
	// AmountUSD 0: an empty bucket is the vendor stating it charged nothing,
	// which is a fact worth reconciling, not an absence. Only a day the vendor
	// left out of its response entirely is omitted, and that still means "not
	// finalized", never zero. Implementations must preserve this distinction —
	// collapsing a reported zero into an omission makes an idle day
	// indistinguishable from an unreconciled one, and leaves a fetch_failed
	// placeholder on such a day permanently unhealable (see reconcileProvider).
	//
	// A non-nil error means the whole fetch failed (transport / auth / decode);
	// the caller records the day(s) as fetch_failed and never fabricates a diff.
	FetchDailyBill(ctx context.Context, from, to time.Time) ([]VendorDailyBill, error)
	// BillingHost is the host this source reads its cost API from, e.g.
	// "api.openai.com". The reconcile job matches it against a Provider row's
	// baseUrl so a provider that merely SHARES the vendor's adapter type — a
	// self-hosted model served over the OpenAI wire format on its own domain —
	// is not reconciled against, and does not double-count, the real vendor's
	// bill. See SameBillingHost.
	BillingHost() string
}

// hostOf extracts the comparable host of a base URL, lowercased and without
// the port. Returns "" when the value is empty or has no host, which callers
// read as "no opinion" rather than as a host that failed to match.
func hostOf(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// SameBillingHost reports whether a Provider row's baseUrl names the same host
// the vendor's cost API is read from, i.e. whether that provider's traffic is
// actually billed by the vendor this source can invoice.
//
// This is the gate that keeps adapter type from being mistaken for vendor
// identity. Provider.adapterType names a WIRE FORMAT, not a company: a
// self-hosted model, a colleague's inference box, or any OpenAI-compatible
// endpoint is configured as adapterType "openai" while costing the OpenAI
// organization nothing. Resolving a bill source from the adapter type alone
// would hand such a provider the real vendor's daily total as its
// vendor_reported_usd — counting the same vendor dollars a second time under a
// provider that never spent them, and manufacturing a drift alert against a
// bill that was never issued.
//
// A baseUrl with no recoverable host matches, rather than being excluded. That
// covers the row saying "use the adapter's own default endpoint", which IS the
// vendor's host, and it points the one ambiguous case — a malformed value — at
// the milder failure: a provider wrongly kept is visible in the report as an
// obviously bogus comparison, while a provider wrongly dropped simply stops
// being reconciled with nothing on the page to say so.
func SameBillingHost(providerBaseURL, billingHost string) bool {
	h := hostOf(providerBaseURL)
	if h == "" {
		return true
	}
	return h == strings.ToLower(billingHost)
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

const (
	// fetchMaxAttempts bounds one request's total tries (initial + retries).
	// The cost APIs are polled once a day per provider, so a handful of tries
	// costs nothing; what it buys is not losing a whole provider's window to a
	// single transient response.
	fetchMaxAttempts = 4
	// fetchBaseBackoff is the first retry delay for a TRANSPORT or 5xx failure;
	// it doubles per attempt up to fetchMaxBackoff. Sub-second is right here:
	// a dropped connection or a single bad gateway is usually over by the time
	// the first retry lands.
	fetchBaseBackoff = 500 * time.Millisecond
	// fetchMaxBackoff caps both the exponential schedule and any Retry-After
	// the vendor supplies. It has to sit ABOVE rateLimitBackoff, or the cap
	// would quietly undo the one wait that matters: at the previous value of
	// 8s it also truncated a vendor-supplied Retry-After pointing a minute
	// out, turning the vendor's own "come back at :00" into another doomed
	// retry 8 seconds later.
	fetchMaxBackoff = 90 * time.Second
	// rateLimitBackoff is the wait after a 429 with no usable Retry-After.
	//
	// It is a full window plus margin, not a doubling step, because OpenAI
	// meters its admin API in FIXED one-minute windows ("30 request(s) every 1
	// minute(s)"). Retrying inside the window can only re-spend a budget that
	// is already exhausted, so the entire exponential schedule — 0.5s, 1s, 2s,
	// 3.5s of total coverage against a 60s window — was structurally unable to
	// outlive the condition it was retrying. Every attempt burned, then the
	// whole provider's window became fetch_failed placeholders (prod
	// 2026-08-16 and 2026-08-18, both at 11:36 UTC; stg 2026-08-01 and
	// 2026-08-05 before that).
	//
	// The limit is per ORGANIZATION, so it is also spent by callers this job
	// never issued — a second deployment sharing the admin key, or an operator
	// in the vendor console. Waiting past the window is what makes the retry
	// answer the actual failure. The job runs once a day; minutes are free.
	rateLimitBackoff = 65 * time.Second
)

// wait pauses for d, or returns early if ctx is cancelled first. Package-level
// so unit tests can replace it and assert the backoff schedule without
// sleeping. Tests that swap it must not run in parallel.
var wait = func(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryable reports whether a status is worth a second try. 429 is the one
// that matters in practice: OpenAI meters its admin API at 30 requests per
// minute across the whole organization, so the costs endpoint can be rate
// limited by traffic this job never issued (observed on stg 2026-08-01 and
// 2026-08-05, each time turning a provider's entire reconcile window into
// fetch_failed placeholder rows). 5xx is the vendor's own transient failure.
// Everything else — 400 malformed query, 401/403 revoked or unscoped admin key
// — is a standing condition that a retry can only repeat.
func retryable(status int) bool {
	return status == http.StatusTooManyRequests || status >= 500
}

// retryAfter reads the Retry-After header, which the vendor sets to the point
// its limit resets. Both RFC forms are accepted: delta-seconds and an HTTP
// date. Returns ok=false when the header is absent or unparseable, leaving the
// caller on its own exponential schedule; a value past fetchMaxBackoff is
// clamped so one hostile header cannot stall the job for the rest of the day.
func retryAfter(h http.Header, now time.Time) (time.Duration, bool) {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0, false
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0, false
		}
		return capBackoff(time.Duration(secs) * time.Second), true
	}
	if at, err := http.ParseTime(v); err == nil {
		d := at.Sub(now)
		if d < 0 {
			d = 0
		}
		return capBackoff(d), true
	}
	return 0, false
}

func capBackoff(d time.Duration) time.Duration {
	if d > fetchMaxBackoff {
		return fetchMaxBackoff
	}
	return d
}

// getJSON performs the request and decodes a 200 JSON body into out, retrying a
// rate-limited or 5xx response with exponential backoff (honouring Retry-After)
// up to fetchMaxAttempts. A non-retryable non-200 status (including 401/403 auth
// failures), and a retryable one that outlived every attempt, are returned as an
// error so the caller maps the day to fetch_failed.
func getJSON(hc *http.Client, req *http.Request, out any) error {
	ctx := req.Context()
	backoff := fetchBaseBackoff
	var lastErr error
	for attempt := 1; ; attempt++ {
		// Clone per attempt: a Request that already travelled through the
		// transport must not be re-sent as-is.
		resp, err := hc.Do(req.Clone(ctx))
		if err != nil {
			// A transport error is indistinguishable from the vendor dropping the
			// connection under load, so it retries on the same schedule.
			lastErr = fmt.Errorf("request: %w", err)
		} else {
			status := resp.StatusCode
			if status == http.StatusOK {
				decErr := json.NewDecoder(resp.Body).Decode(out)
				_ = resp.Body.Close()
				if decErr != nil {
					return fmt.Errorf("decode: %w", decErr)
				}
				return nil
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
			hdr := resp.Header
			_ = resp.Body.Close()
			if !retryable(status) {
				return fmt.Errorf("status %d: %s", status, string(body))
			}
			lastErr = fmt.Errorf("status %d: %s", status, string(body))
			// A rate limit is a timed condition, not a transient one: jump the
			// schedule past the vendor's window instead of spending attempts
			// inside it. Guarded so a schedule that has already grown beyond a
			// window is never pulled back DOWN to one.
			if status == http.StatusTooManyRequests && backoff < rateLimitBackoff {
				backoff = rateLimitBackoff
			}
			// The vendor's own reset time wins over both schedules when it
			// gives one — it is the only value that is not a guess.
			if d, ok := retryAfter(hdr, time.Now()); ok {
				backoff = d
			}
		}
		if attempt >= fetchMaxAttempts {
			return fmt.Errorf("%w (after %d attempts)", lastErr, attempt)
		}
		if err := wait(ctx, backoff); err != nil {
			return fmt.Errorf("%w (retry aborted: %w)", lastErr, err)
		}
		backoff = capBackoff(backoff * 2)
	}
}
