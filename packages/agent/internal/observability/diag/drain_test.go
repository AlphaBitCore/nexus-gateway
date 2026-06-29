package diag

import (
	"bytes"
	"context"
	"database/sql"
	"github.com/goccy/go-json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	opsmetrics "github.com/AlphaBitCore/nexus-gateway/packages/shared/core/metrics/registry"
)

// drainHandlerInput captures the request the agent sent and the response the
// fake Hub returned, so the test can introspect both sides of the contract
// without re-implementing JSON parsing in each case.
type drainHandlerInput struct {
	Events []map[string]any `json:"events"`
}

func newDrainTestServer(t *testing.T, behaviour func(call int, in drainHandlerInput) (status int, body any)) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if got := r.URL.Path; got != "/api/internal/things/diag-events:batch" {
			t.Errorf("path = %q, want /api/internal/things/diag-events:batch", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer device-token-xyz" {
			t.Errorf("Authorization = %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q", got)
		}

		body, _ := io.ReadAll(r.Body)
		var in drainHandlerInput
		if err := json.Unmarshal(body, &in); err != nil {
			t.Errorf("unmarshal request: %v", err)
		}
		callIdx := int(atomic.AddInt32(&calls, 1)) - 1
		status, resp := behaviour(callIdx, in)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func seedBuffer(t *testing.T, buf *LocalBuffer, n int) []string {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Second)
	for i := range n {
		evt := opsmetrics.DiagEvent{
			ThingID:     "thing-1",
			OccurredAt:  base.Add(time.Duration(i) * time.Second),
			Level:       opsmetrics.LevelFatal,
			EventType:   opsmetrics.EventTypeCrash,
			Source:      "main",
			Message:     "boom",
			MessageHash: "h",
			RepeatCount: 1,
		}
		if err := buf.Insert(evt); err != nil {
			t.Fatalf("seed insert %d: %v", i, err)
		}
	}
	rows, err := buf.List(n)
	if err != nil {
		t.Fatalf("seed list: %v", err)
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	return ids
}

func TestDrainPending_PostsAllAndDeletes(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)
	ids := seedBuffer(t, buf, 3)

	srv, calls := newDrainTestServer(t, func(_ int, in drainHandlerInput) (int, any) {
		// Echo every id back as accepted.
		acc := make([]string, 0, len(in.Events))
		for _, e := range in.Events {
			if id, ok := e["id"].(string); ok {
				acc = append(acc, id)
			}
		}
		return http.StatusOK, map[string]any{"acceptedIds": acc}
	})

	cfg := DrainConfig{
		Buffer:      buf,
		HTTPClient:  srv.Client(),
		HubURL:      srv.URL,
		DeviceToken: "device-token-xyz",
		BatchSize:   10,
	}
	if err := DrainPending(context.Background(), cfg); err != nil {
		t.Fatalf("DrainPending: %v", err)
	}

	// All rows pruned after ack.
	count, _ := buf.Pending()
	if count != 0 {
		t.Errorf("Pending after drain = %d, want 0; ids=%v", count, ids)
	}
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Errorf("server calls = %d, want 1 (single batch)", got)
	}
}

func TestDrainPending_PartialAckLoopsUntilDone(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)
	_ = seedBuffer(t, buf, 3)

	srv, calls := newDrainTestServer(t, func(call int, in drainHandlerInput) (int, any) {
		// First call: ack only the first event. Second call: ack all of
		// the remaining events.
		if call == 0 {
			if len(in.Events) == 0 {
				return http.StatusOK, map[string]any{"acceptedIds": []string{}}
			}
			id := in.Events[0]["id"].(string)
			return http.StatusOK, map[string]any{"acceptedIds": []string{id}}
		}
		acc := make([]string, 0, len(in.Events))
		for _, e := range in.Events {
			acc = append(acc, e["id"].(string))
		}
		return http.StatusOK, map[string]any{"acceptedIds": acc}
	})

	cfg := DrainConfig{
		Buffer:      buf,
		HTTPClient:  srv.Client(),
		HubURL:      srv.URL,
		DeviceToken: "device-token-xyz",
		BatchSize:   10,
	}
	if err := DrainPending(context.Background(), cfg); err != nil {
		t.Fatalf("DrainPending: %v", err)
	}

	count, _ := buf.Pending()
	if count != 0 {
		t.Errorf("Pending after drain = %d, want 0", count)
	}
	if got := atomic.LoadInt32(calls); got < 2 {
		t.Errorf("server calls = %d, want >=2 (partial-ack loop)", got)
	}
}

func TestDrainPending_HubReturnsZeroBumpsAttempts(t *testing.T) {
	buf, db := newTestLocalBuffer(t)
	_ = seedBuffer(t, buf, 2)

	srv, _ := newDrainTestServer(t, func(_ int, _ drainHandlerInput) (int, any) {
		return http.StatusOK, map[string]any{"acceptedIds": []string{}}
	})

	cfg := DrainConfig{
		Buffer:      buf,
		HTTPClient:  srv.Client(),
		HubURL:      srv.URL,
		DeviceToken: "device-token-xyz",
		BatchSize:   10,
	}
	err := DrainPending(context.Background(), cfg)
	if err == nil {
		t.Fatalf("expected error when Hub accepts 0/N")
	}

	// Rows still present, attempts incremented.
	count, _ := buf.Pending()
	if count != 2 {
		t.Errorf("Pending = %d, want 2 (rows preserved on zero ack)", count)
	}
	rows, err := db.Query("SELECT attempts FROM pending_diag_event")
	if err != nil {
		t.Fatalf("attempts query: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	for rows.Next() {
		var a int
		_ = rows.Scan(&a)
		if a != 1 {
			t.Errorf("attempts = %d, want 1", a)
		}
	}
}

func TestDrainPending_NoOpWhenBufferEmpty(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusInternalServerError) // would fail if hit
	}))
	defer srv.Close()

	cfg := DrainConfig{
		Buffer:      buf,
		HTTPClient:  srv.Client(),
		HubURL:      srv.URL,
		DeviceToken: "device-token-xyz",
		BatchSize:   10,
	}
	if err := DrainPending(context.Background(), cfg); err != nil {
		t.Fatalf("DrainPending: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("server hits = %d, want 0 for empty buffer", got)
	}
}

func TestDrainPending_PostsExpectedWireFormat(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)
	_ = seedBuffer(t, buf, 1)

	var captured drainHandlerInput
	srv, _ := newDrainTestServer(t, func(_ int, in drainHandlerInput) (int, any) {
		captured = in
		acc := make([]string, 0, len(in.Events))
		for _, e := range in.Events {
			acc = append(acc, e["id"].(string))
		}
		return http.StatusOK, map[string]any{"acceptedIds": acc}
	})

	cfg := DrainConfig{
		Buffer:      buf,
		HTTPClient:  srv.Client(),
		HubURL:      srv.URL,
		DeviceToken: "device-token-xyz",
		BatchSize:   10,
	}
	if err := DrainPending(context.Background(), cfg); err != nil {
		t.Fatalf("DrainPending: %v", err)
	}

	if len(captured.Events) != 1 {
		t.Fatalf("captured events = %d, want 1", len(captured.Events))
	}
	got := captured.Events[0]
	if _, ok := got["id"].(string); !ok || got["id"] == "" {
		t.Errorf("id missing/empty: %+v", got)
	}
	if got["level"] != opsmetrics.LevelFatal {
		t.Errorf("level = %v, want fatal", got["level"])
	}
	if got["eventType"] != opsmetrics.EventTypeCrash {
		t.Errorf("eventType = %v, want crash", got["eventType"])
	}
}

// TestDrainPending_GuardsRequireDeps covers the four boot-time
// guards in DrainPending — each must surface a descriptive error so
// an operator misconfiguration doesn't silently fail at agent start.
func TestDrainPending_GuardsRequireDeps(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)

	cases := []struct {
		name string
		cfg  DrainConfig
		want string
	}{
		{"nil buffer", DrainConfig{}, "nil buffer"},
		{"nil http client", DrainConfig{Buffer: buf}, "nil http client"},
		{"empty hub url", DrainConfig{Buffer: buf, HTTPClient: http.DefaultClient}, "empty hub url"},
		{"empty device token", DrainConfig{Buffer: buf, HTTPClient: http.DefaultClient, HubURL: "http://hub"}, "empty device token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := DrainPending(context.Background(), tc.cfg)
			if err == nil {
				t.Fatalf("expected guard error for %s", tc.name)
			}
			if !containsStr(err.Error(), tc.want) {
				t.Errorf("error missing %q: %v", tc.want, err)
			}
		})
	}
}

// TestDrainPending_NewRequestError covers the build-drain-request
// failure branch — a URL with a control byte fails before transport.
func TestDrainPending_NewRequestError(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)
	must(t, buf.Insert(makeDiagEvent("e1", time.Now().UTC(), opsmetrics.LevelFatal)))

	err := DrainPending(context.Background(), DrainConfig{
		Buffer:      buf,
		HTTPClient:  http.DefaultClient,
		HubURL:      "http://\x7f",
		DeviceToken: "tok",
	})
	if err == nil {
		t.Fatal("expected build-drain-request error")
	}
}

// TestDrainPending_TransportErrorBumpsAttempts covers the
// http.Client.Do error branch — the row must stay in the buffer AND
// IncrAttempts must run so the next agent start can show "this row
// never reached Hub" distinctly from "Hub rejected". Attempts column
// is read directly via SQL since PendingDiagRow doesn't surface it.
func TestDrainPending_TransportErrorBumpsAttempts(t *testing.T) {
	buf, db := newTestLocalBuffer(t)
	must(t, buf.Insert(makeDiagEvent("e1", time.Now().UTC(), opsmetrics.LevelFatal)))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	dead := srv.URL
	srv.Close()

	err := DrainPending(context.Background(), DrainConfig{
		Buffer:      buf,
		HTTPClient:  &http.Client{Timeout: 500 * time.Millisecond},
		HubURL:      dead,
		DeviceToken: "tok",
	})
	if err == nil {
		t.Fatal("expected transport error")
	}
	rows, _ := buf.List(10)
	if len(rows) != 1 {
		t.Fatalf("row should stay in buffer; got %d", len(rows))
	}
	if attempts := queryAttempts(t, db); attempts == 0 {
		t.Errorf("attempts should be bumped after transport err; got %d", attempts)
	}
}

// TestDrainPending_Non200BumpsAttempts covers the status-not-200
// branch.
func TestDrainPending_Non200BumpsAttempts(t *testing.T) {
	buf, db := newTestLocalBuffer(t)
	must(t, buf.Insert(makeDiagEvent("e1", time.Now().UTC(), opsmetrics.LevelFatal)))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := DrainPending(context.Background(), DrainConfig{
		Buffer:      buf,
		HTTPClient:  http.DefaultClient,
		HubURL:      srv.URL,
		DeviceToken: "tok",
	})
	if err == nil {
		t.Fatal("expected non-200 error")
	}
	rows, _ := buf.List(10)
	if len(rows) != 1 {
		t.Errorf("non-200 must keep row; got %d rows", len(rows))
	}
	if attempts := queryAttempts(t, db); attempts == 0 {
		t.Errorf("attempts should be bumped after non-200; got %d", attempts)
	}
}

// queryAttempts is a tiny SQL helper that reads the max attempts
// across all pending rows. The PendingDiagRow shape doesn't surface
// this column today; reading directly is the simplest way to assert
// IncrAttempts behavior without changing public API.
func queryAttempts(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COALESCE(MAX(attempts), 0) FROM pending_diag_event`).Scan(&n); err != nil {
		t.Fatalf("query attempts: %v", err)
	}
	return n
}

// containsStr is a tiny strings.Contains shim.
func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestDrainPending_ListErrorSurfaces covers the `cfg.Buffer.List(...) err`
// branch — if the underlying SQLCipher handle is closed, DrainPending must
// return a "list pending" error rather than panicking or silently
// returning.
func TestDrainPending_ListErrorSurfaces(t *testing.T) {
	buf, db := newTestLocalBuffer(t)
	_ = db.Close() // forces List to error on the very first call.

	err := DrainPending(context.Background(), DrainConfig{
		Buffer:      buf,
		HTTPClient:  http.DefaultClient,
		HubURL:      "http://hub",
		DeviceToken: "tok",
	})
	if err == nil {
		t.Fatal("expected list error")
	}
	if !containsStr(err.Error(), "list pending") {
		t.Errorf("error missing wrap: %v", err)
	}
}

// TestDrainPending_ThingIDHeader exercises the optional X-Thing-Id
// branch. The Hub-side handler can fall back to this header when the
// device-token context is unavailable, so dropping it silently would
// break a documented contract.
func TestDrainPending_ThingIDHeader(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)
	_ = seedBuffer(t, buf, 1)

	var sawThingID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawThingID = r.Header.Get("X-Thing-Id")
		body, _ := io.ReadAll(r.Body)
		var in drainHandlerInput
		_ = json.Unmarshal(body, &in)
		acc := make([]string, 0, len(in.Events))
		for _, e := range in.Events {
			acc = append(acc, e["id"].(string))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"acceptedIds": acc})
	}))
	t.Cleanup(srv.Close)

	cfg := DrainConfig{
		Buffer:      buf,
		HTTPClient:  srv.Client(),
		HubURL:      srv.URL,
		DeviceToken: "device-token-xyz",
		ThingID:     "thing-from-config-xyz",
		BatchSize:   10,
	}
	if err := DrainPending(context.Background(), cfg); err != nil {
		t.Fatalf("DrainPending: %v", err)
	}
	if sawThingID != "thing-from-config-xyz" {
		t.Errorf("X-Thing-Id header = %q, want thing-from-config-xyz", sawThingID)
	}
}

// TestDrainPending_BadAckBodyErrors covers the JSON decode error branch
// for the Hub response — a 200 OK with a non-JSON body must surface a
// "decode drain ack" error so we don't silently treat invalid responses
// as zero-ack.
func TestDrainPending_BadAckBodyErrors(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)
	_ = seedBuffer(t, buf, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("this is not json {{{"))
	}))
	t.Cleanup(srv.Close)

	err := DrainPending(context.Background(), DrainConfig{
		Buffer:      buf,
		HTTPClient:  srv.Client(),
		HubURL:      srv.URL,
		DeviceToken: "tok",
	})
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !containsStr(err.Error(), "decode drain ack") {
		t.Errorf("error missing wrap: %v", err)
	}

	// Row must remain — decode error is before Delete.
	rows, _ := buf.List(10)
	if len(rows) != 1 {
		t.Errorf("decode error must keep row; got %d", len(rows))
	}
}

// TestDrainPending_DeleteAfterAckErrors covers the post-ack Delete
// failure branch — if the Hub successfully accepts a row but the local
// Delete fails (closed DB simulates a SQLCipher I/O failure), we must
// surface "delete acked rows" rather than silently looping forever.
func TestDrainPending_DeleteAfterAckErrors(t *testing.T) {
	buf, db := newTestLocalBuffer(t)
	_ = seedBuffer(t, buf, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var in drainHandlerInput
		_ = json.Unmarshal(body, &in)
		acc := make([]string, 0, len(in.Events))
		for _, e := range in.Events {
			acc = append(acc, e["id"].(string))
		}
		// Close DB right before responding so Delete (called after ack)
		// returns an error from the SQL layer.
		_ = db.Close()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"acceptedIds": acc})
	}))
	t.Cleanup(srv.Close)

	err := DrainPending(context.Background(), DrainConfig{
		Buffer:      buf,
		HTTPClient:  srv.Client(),
		HubURL:      srv.URL,
		DeviceToken: "tok",
	})
	if err == nil {
		t.Fatal("expected delete error")
	}
	if !containsStr(err.Error(), "delete acked rows") {
		t.Errorf("error missing wrap: %v", err)
	}
}

// TestDrainPending_IncrAttemptsWarns exercises the three
// `logger.Warn("incr attempts after ...")` fallback branches by
// closing the SQLCipher DB right before IncrAttempts is invoked
// inside the drain loop, then asserting the corresponding warn line
// reaches the logger and the upstream error is still propagated to
// the caller. Each subtest hits a distinct upstream failure mode
// (transport, non-200, zero-ack) so the three nearly-identical warn
// strings in drain.go can't silently drift apart.
func TestDrainPending_IncrAttemptsWarns(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(t *testing.T, db *sql.DB) (httpClient *http.Client, hubURL string)
		wantWarn string
		wantErr  string
	}{
		{
			name: "transport error",
			setup: func(t *testing.T, db *sql.DB) (*http.Client, string) {
				t.Helper()
				client := &http.Client{
					Transport: roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
						// At this point List has already returned the row; we
						// close the DB so the post-error IncrAttempts fails.
						_ = db.Close()
						return nil, errSimulatedTransport
					}),
				}
				return client, "http://hub"
			},
			wantWarn: "incr attempts after transport error",
			wantErr:  "post diag drain",
		},
		{
			name: "non-200",
			setup: func(t *testing.T, db *sql.DB) (*http.Client, string) {
				t.Helper()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_ = db.Close()
					http.Error(w, "boom", http.StatusBadGateway)
				}))
				t.Cleanup(srv.Close)
				return srv.Client(), srv.URL
			},
			wantWarn: "incr attempts after non-200",
			wantErr:  "diag drain: status 502",
		},
		{
			name: "zero ack",
			setup: func(t *testing.T, db *sql.DB) (*http.Client, string) {
				t.Helper()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_ = db.Close()
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"acceptedIds": []string{}})
				}))
				t.Cleanup(srv.Close)
				return srv.Client(), srv.URL
			},
			wantWarn: "incr attempts after zero ack",
			wantErr:  "hub accepted 0/",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf, db := newTestLocalBuffer(t)
			_ = seedBuffer(t, buf, 1)

			var logBuf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))

			client, hubURL := tc.setup(t, db)
			err := DrainPending(context.Background(), DrainConfig{
				Buffer:      buf,
				HTTPClient:  client,
				HubURL:      hubURL,
				DeviceToken: "tok",
				Log:         logger,
			})
			if err == nil {
				t.Fatalf("expected drain error for %s", tc.name)
			}
			if !containsStr(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want substr %q", err, tc.wantErr)
			}
			if !containsStr(logBuf.String(), tc.wantWarn) {
				t.Errorf("logger missing %q; got: %s", tc.wantWarn, logBuf.String())
			}
		})
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

var errSimulatedTransport = &netErr{msg: "simulated transport error"}

type netErr struct{ msg string }

func (e *netErr) Error() string { return e.msg }

// TestInsert_MarshalErrorBubblesUp covers the json.Marshal failure
// branch in Insert by handing it a DiagEvent whose Attrs holds an
// unmarshalable type (a channel). Observable behavior: Insert returns
// a wrapped "marshal diag payload" error and no row is persisted.
func TestInsert_MarshalErrorBubblesUp(t *testing.T) {
	buf, _ := newTestLocalBuffer(t)
	evt := opsmetrics.DiagEvent{
		ThingID:     "thing-1",
		OccurredAt:  time.Now().UTC(),
		Level:       opsmetrics.LevelFatal,
		EventType:   opsmetrics.EventTypeCrash,
		Source:      "main",
		Message:     "bad",
		MessageHash: "h",
		// channels are not JSON-marshalable, forcing json.Marshal to error.
		Attrs:       map[string]any{"chan": make(chan int)},
		RepeatCount: 1,
	}
	err := buf.Insert(evt)
	if err == nil {
		t.Fatal("expected marshal error")
	}
	if !containsStr(err.Error(), "marshal diag payload") {
		t.Errorf("err missing wrap: %v", err)
	}
	count, _ := buf.Pending()
	if count != 0 {
		t.Errorf("no row should be persisted; got %d", count)
	}
}
