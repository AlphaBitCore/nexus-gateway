package vendorbill

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// recordWaits replaces the backoff seam with one that records the requested
// delays instead of sleeping, and restores it when the test ends. Tests using
// it must not run in parallel — the seam is package-level.
func recordWaits(t *testing.T) *[]time.Duration {
	t.Helper()
	var got []time.Duration
	prev := wait
	wait = func(_ context.Context, d time.Duration) error {
		got = append(got, d)
		return nil
	}
	t.Cleanup(func() { wait = prev })
	return &got
}

// TestFetchDailyBill_RetriesRateLimitAndSucceeds is the incident this retry
// exists for: OpenAI meters its admin API at 30 requests per minute across the
// whole organization, so the costs endpoint can answer 429 because of traffic
// this job never issued. Before the retry, that single response turned every
// day of the provider's reconcile window into a fetch_failed placeholder
// (observed on stg 2026-08-01 and 2026-08-05).
func TestFetchDailyBill_RetriesRateLimitAndSucceeds(t *testing.T) {
	waits := recordWaits(t)
	var calls int
	src := openaiServer(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"code":"rate_limit_exceeded"}}`))
			return
		}
		w.Write([]byte(`{"data":[{"start_time":` + itoa(utcDay(2026, 8, 3).Unix()) + `,
		  "results":[{"amount":{"value":1.81,"currency":"usd"},"project_id":"proj_gw"}]}],
		  "has_more":false,"next_page":null}`))
	})

	bills, err := src.FetchDailyBill(context.Background(), utcDay(2026, 8, 3), utcDay(2026, 8, 3))
	if err != nil {
		t.Fatalf("FetchDailyBill: %v", err)
	}
	if calls != 2 {
		t.Errorf("server calls = %d, want 2 (one rate-limited, one served)", calls)
	}
	if len(bills) != 1 || bills[0].AmountUSD != 1.81 {
		t.Fatalf("bills = %+v, want the day the retry recovered", bills)
	}
	// The wait is a full rate-limit window, not the exponential base: the
	// vendor meters in fixed one-minute windows, so a sub-second retry can only
	// re-spend a budget that is already gone.
	if len(*waits) != 1 || (*waits)[0] != rateLimitBackoff {
		t.Errorf("backoff schedule = %v, want one wait of %v", *waits, rateLimitBackoff)
	}
	if rateLimitBackoff <= time.Minute {
		t.Errorf("rateLimitBackoff = %v, must exceed the vendor's 1-minute window or the retry cannot outlive it", rateLimitBackoff)
	}
}

// TestGetJSON_RateLimitScheduleOutlivesTheVendorWindow pins the property the
// 429 branch exists for: the retry schedule must still be waiting after the
// vendor's one-minute limit window has elapsed. The earlier schedule (0.5s, 1s,
// 2s) gave up 56 seconds early and burned every attempt inside the window,
// which is how a single 429 turned a whole provider's reconcile window into
// fetch_failed placeholders on prod (2026-08-16, 2026-08-18).
func TestGetJSON_RateLimitScheduleOutlivesTheVendorWindow(t *testing.T) {
	waits := recordWaits(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`slow down`))
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err := getJSON(srv.Client(), req, &struct{}{}); err == nil {
		t.Fatal("expected the exhausted retry to return an error")
	}
	var total time.Duration
	for _, w := range *waits {
		total += w
	}
	const vendorWindow = time.Minute
	if (*waits)[0] < vendorWindow {
		t.Errorf("first 429 wait = %v, want at least the vendor's %v window", (*waits)[0], vendorWindow)
	}
	if total <= vendorWindow {
		t.Errorf("total backoff = %v, want more than the %v window so a limit that stays hot is still covered", total, vendorWindow)
	}
}

// TestGetJSON_BackoffDoublesAndGivesUp: a vendor fault that stays hot must not
// be hammered, and must not retry forever either — the job is daily, so the
// window is better left to the next run than to a request that never returns.
//
// Driven by 503 rather than 429 on purpose: the exponential schedule is the
// answer to a TRANSIENT fault, where a sub-second first retry is the right
// guess. A rate limit is a timed condition and takes the fixed
// rateLimitBackoff instead — asserted by
// TestGetJSON_RateLimitScheduleOutlivesTheVendorWindow.
func TestGetJSON_BackoffDoublesAndGivesUp(t *testing.T) {
	waits := recordWaits(t)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`upstream down`))
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	err := getJSON(srv.Client(), req, &struct{}{})
	if err == nil {
		t.Fatal("expected the exhausted retry to return an error")
	}
	if !strings.Contains(err.Error(), "status 503") || !strings.Contains(err.Error(), "after 4 attempts") {
		t.Errorf("error = %q, want it to carry the vendor status and the attempt count", err)
	}
	if calls != fetchMaxAttempts {
		t.Errorf("server calls = %d, want %d", calls, fetchMaxAttempts)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}
	if len(*waits) != len(want) {
		t.Fatalf("waits = %v, want %v", *waits, want)
	}
	for i, w := range want {
		if (*waits)[i] != w {
			t.Errorf("wait[%d] = %v, want %v", i, (*waits)[i], w)
		}
	}
}

// TestGetJSON_HonoursRetryAfter: the vendor knows when its limit resets, so its
// header wins over the local schedule.
func TestGetJSON_HonoursRetryAfter(t *testing.T) {
	waits := recordWaits(t)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err := getJSON(srv.Client(), req, &struct{}{}); err != nil {
		t.Fatalf("getJSON: %v", err)
	}
	if len(*waits) != 1 || (*waits)[0] != 3*time.Second {
		t.Errorf("waits = %v, want a single 3s wait taken from Retry-After", *waits)
	}
}

// TestGetJSON_DoesNotRetryStandingFailures: a revoked or unscoped admin key
// answers the same way every time. Retrying it would delay the fetch_failed row
// the operator needs to see, for no chance of a different answer.
func TestGetJSON_DoesNotRetryStandingFailures(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			waits := recordWaits(t)
			var calls int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(status)
				w.Write([]byte(`denied`))
			}))
			defer srv.Close()

			req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
			err := getJSON(srv.Client(), req, &struct{}{})
			if err == nil || !strings.Contains(err.Error(), "denied") {
				t.Fatalf("err = %v, want the vendor body surfaced", err)
			}
			if strings.Contains(err.Error(), "attempts") {
				t.Errorf("err = %v, want no retry-exhaustion wording on a standing failure", err)
			}
			if calls != 1 || len(*waits) != 0 {
				t.Errorf("calls = %d, waits = %v, want exactly one call and no backoff", calls, *waits)
			}
		})
	}
}

// TestGetJSON_RetriesServerErrorAndTransportFailure: 5xx and a dropped
// connection are the vendor's transient failures, on the same schedule as 429.
func TestGetJSON_RetriesServerErrorAndTransportFailure(t *testing.T) {
	recordWaits(t)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		switch calls {
		case 1:
			w.WriteHeader(http.StatusBadGateway)
		case 2:
			// Drop the connection mid-response: a transport error, not a status.
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Error("hijack unsupported")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
		default:
			w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer srv.Close()

	var out struct {
		OK bool `json:"ok"`
	}
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if err := getJSON(srv.Client(), req, &out); err != nil {
		t.Fatalf("getJSON: %v", err)
	}
	if !out.OK || calls != 3 {
		t.Errorf("out.OK = %v after %d calls, want the third response decoded", out.OK, calls)
	}
}

// TestGetJSON_ContextCancellationStopsRetrying: a shutdown must not be held up
// by a backoff, and the error must still name the vendor status that caused it.
func TestGetJSON_ContextCancellationStopsRetrying(t *testing.T) {
	prev := wait
	wait = func(context.Context, time.Duration) error { return context.Canceled }
	t.Cleanup(func() { wait = prev })

	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	err := getJSON(srv.Client(), req, &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "status 503") || !strings.Contains(err.Error(), "retry aborted") {
		t.Fatalf("err = %v, want the 503 plus the aborted-retry reason", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want the cancellation to stop after the first attempt", calls)
	}
}

// TestGetJSON_DecodeErrorIsNotRetried: a 200 with an unparseable body is a
// contract break, not a transient condition.
func TestGetJSON_DecodeErrorIsNotRetried(t *testing.T) {
	waits := recordWaits(t)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	err := getJSON(srv.Client(), req, &struct{}{})
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Fatalf("err = %v, want a decode error", err)
	}
	if calls != 1 || len(*waits) != 0 {
		t.Errorf("calls = %d, waits = %v, want no retry on a decode failure", calls, *waits)
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		header string
		want   time.Duration
		wantOK bool
	}{
		{"absent", "", 0, false},
		{"delta seconds", "5", 5 * time.Second, true},
		{"delta seconds clamped", "600", fetchMaxBackoff, true},
		{"negative seconds ignored", "-1", 0, false},
		{"http date", "Thu, 06 Aug 2026 12:00:04 GMT", 4 * time.Second, true},
		{"http date already passed", "Thu, 06 Aug 2026 11:59:00 GMT", 0, true},
		{"http date clamped", "Thu, 06 Aug 2026 13:00:00 GMT", fetchMaxBackoff, true},
		{"garbage", "soon", 0, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			if c.header != "" {
				h.Set("Retry-After", c.header)
			}
			got, ok := retryAfter(h, now)
			if ok != c.wantOK || got != c.want {
				t.Errorf("retryAfter(%q) = (%v, %v), want (%v, %v)", c.header, got, ok, c.want, c.wantOK)
			}
		})
	}
}

// TestWait_ReturnsOnContextCancellation covers the production backoff seam the
// other tests replace: a shutdown must interrupt a pending retry delay rather
// than hold the job for the full backoff.
func TestWait_ReturnsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait = %v, want context.Canceled without sleeping", err)
	}
	if err := wait(context.Background(), time.Millisecond); err != nil {
		t.Fatalf("wait = %v, want nil after the timer fired", err)
	}
	if err := wait(context.Background(), 0); err != nil {
		t.Fatalf("wait(0) = %v, want an immediate nil", err)
	}
}
