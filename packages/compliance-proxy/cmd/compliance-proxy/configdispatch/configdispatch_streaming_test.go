package configdispatch_test

import (
	"errors"
	"testing"

	sqlmock "github.com/DATA-DOG/go-sqlmock"

	"github.com/AlphaBitCore/nexus-gateway/packages/compliance-proxy/cmd/compliance-proxy/configdispatch"
	streampolicy "github.com/AlphaBitCore/nexus-gateway/packages/shared/transport/streaming/policy"
)

const streamingPolicyQuery = `SELECT value FROM system_metadata WHERE key = $1`

// TestStreamingCompliance_EmptyTriggerReReadsSystemMetadata is the regression
// guard for R-7.
//
// streaming_compliance is a Type-B key: the Hub pushes JSON null on every push.
// This handler used to hand that null to ApplyShadowState, which decoded it into
// DefaultPolicy() — passthrough, which neither accumulates nor can reject.
// Measured on the proxy: boot installed the admin's chunked_async at
// 10:30:50.045 and this handler replaced it with passthrough 70 ms later, after
// which every SSE stream was relayed uninspected.
//
// The contract, stated in configuration-architecture.md and implemented by the
// sibling registerPayloadCapture: a trigger means RE-READ.
func TestStreamingCompliance_EmptyTriggerReReadsSystemMetadata(t *testing.T) {
	for _, payload := range []string{"null", "", "{}"} {
		t.Run("payload="+payload, func(t *testing.T) {
			db, mock := newSQLMock(t)
			d := silentDeps(t)
			d.ConfigDB = db
			// Seed the store with the WRONG mode so a no-op would be visible.
			d.StreamingPolicyStore = streampolicy.NewStore(streampolicy.Policy{Mode: streampolicy.ModePassThrough})

			mock.ExpectQuery(streamingPolicyQuery).
				WithArgs(streampolicy.SystemMetadataKey).
				WillReturnRows(sqlmock.NewRows([]string{"value"}).
					AddRow([]byte(`{"default_mode":"chunked_async","chunk_bytes":4096}`)))

			loader := configdispatch.BuildConfigLoader(d)
			if err := applyKey(t, loader, "streaming_compliance", []byte(payload)); err != nil {
				t.Fatalf("apply streaming_compliance: %v", err)
			}
			if got := d.StreamingPolicyStore.Get().Mode; got != streampolicy.ModeChunkedAsync {
				t.Errorf("mode = %q after an empty trigger, want chunked_async — the handler must re-read "+
					"system_metadata, not decode the empty payload into defaults", got)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("the re-read never happened: %v", err)
			}
		})
	}
}

// TestStreamingCompliance_ReReadFailureKeepsCurrentPolicy: a transient DB error
// must not disable stream inspection. Reverting to defaults on a read failure
// would turn a blip in the database into silently unwatched traffic.
func TestStreamingCompliance_ReReadFailureKeepsCurrentPolicy(t *testing.T) {
	db, mock := newSQLMock(t)
	d := silentDeps(t)
	d.ConfigDB = db
	d.StreamingPolicyStore = streampolicy.NewStore(streampolicy.Policy{Mode: streampolicy.ModeChunkedAsync})

	mock.ExpectQuery(streamingPolicyQuery).
		WithArgs(streampolicy.SystemMetadataKey).
		WillReturnError(errors.New("connection reset"))

	loader := configdispatch.BuildConfigLoader(d)
	if err := applyKey(t, loader, "streaming_compliance", []byte("null")); err != nil {
		t.Fatalf("a re-read failure must not fail the apply: %v", err)
	}
	if got := d.StreamingPolicyStore.Get().Mode; got != streampolicy.ModeChunkedAsync {
		t.Errorf("mode = %q after a failed re-read, want chunked_async kept", got)
	}
}

// TestStreamingCompliance_NoRowKeepsCurrentPolicy: nothing configured anywhere
// is not the same as "configured to the default".
func TestStreamingCompliance_NoRowKeepsCurrentPolicy(t *testing.T) {
	db, mock := newSQLMock(t)
	d := silentDeps(t)
	d.ConfigDB = db
	d.StreamingPolicyStore = streampolicy.NewStore(streampolicy.Policy{Mode: streampolicy.ModeBufferFullBlock})

	mock.ExpectQuery(streamingPolicyQuery).
		WithArgs(streampolicy.SystemMetadataKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}))

	loader := configdispatch.BuildConfigLoader(d)
	if err := applyKey(t, loader, "streaming_compliance", []byte("null")); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := d.StreamingPolicyStore.Get().Mode; got != streampolicy.ModeBufferFullBlock {
		t.Errorf("mode = %q, want buffer_full_block kept when no row exists", got)
	}
}

// TestStreamingCompliance_PayloadWithStateAppliesWithoutReRead — the other
// direction. A handler that always re-read would make a genuine Type-A push a
// no-op, which is the same defect facing the other way.
func TestStreamingCompliance_PayloadWithStateAppliesWithoutReRead(t *testing.T) {
	db, mock := newSQLMock(t)
	d := silentDeps(t)
	d.ConfigDB = db
	d.StreamingPolicyStore = streampolicy.NewStore(streampolicy.Policy{Mode: streampolicy.ModePassThrough})
	// No ExpectQuery: any DB read here is a failure of the test's premise.

	loader := configdispatch.BuildConfigLoader(d)
	if err := applyKey(t, loader, "streaming_compliance",
		[]byte(`{"default_mode":"buffer_full_block"}`)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := d.StreamingPolicyStore.Get().Mode; got != streampolicy.ModeBufferFullBlock {
		t.Errorf("mode = %q, want buffer_full_block — a payload carrying state must be applied directly", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB activity for a payload that carried state: %v", err)
	}
}
