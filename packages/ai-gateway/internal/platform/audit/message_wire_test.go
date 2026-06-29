package audit

import (
	"encoding/json"
	"reflect"
	"testing"
)

// jsonbMap marshals a producer JSONB field (identity / details — now typed
// structs on the ai-gateway path) and unmarshals it back into the generic
// map[string]any shape that actually lands in the Postgres JSONB column. Tests
// assert on this post-serialization view because it is exactly what the Hub
// consumer writes and the UI reads — strictly more faithful than inspecting the
// in-memory struct. JSON numbers decode as float64 here.
func jsonbMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal JSONB field: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal JSONB field %q: %v", raw, err)
	}
	return m
}

// TestBuildIdentity_WireShape pins the serialized identity object for each
// resolution combination. The typed identityWire must reproduce the old
// map[string]any build key-for-key: a subtree key is present iff its foreign key
// resolved, and status is matched iff an owner (user or project) resolved.
func TestBuildIdentity_WireShape(t *testing.T) {
	cases := []struct {
		name string
		rec  Record
		want map[string]any
	}{
		{
			name: "personal_vk_user_and_credential",
			rec: Record{
				VirtualKeyID: "vk1", VirtualKeyName: "VK One",
				UserID: "u1", UserDisplayName: "Alice",
				CredentialID: "c1", CredentialName: "Cred",
			},
			want: map[string]any{
				"vk":            map[string]any{"id": "vk1", "name": "VK One"},
				"user":          map[string]any{"id": "u1", "name": "Alice"},
				"apiCredential": map[string]any{"id": "c1", "name": "Cred"},
				"status":        "matched",
			},
		},
		{
			name: "application_vk_project_resolved_no_user",
			rec: Record{
				VirtualKeyID: "vk2", VirtualKeyName: "App VK",
				ProjectID: "p1", ProjectName: "Research",
			},
			want: map[string]any{
				"vk":      map[string]any{"id": "vk2", "name": "App VK"},
				"project": map[string]any{"id": "p1", "name": "Research"},
				"status":  "matched",
			},
		},
		{
			name: "nothing_resolved_is_pending_with_only_status",
			rec:  Record{},
			want: map[string]any{"status": "pending"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := jsonbMap(t, buildIdentity(&tc.rec))
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("identity JSONB mismatch\n got: %#v\nwant: %#v", got, tc.want)
			}
		})
	}
}

// TestBuildDetails_WireShape pins the serialized details object. The ten base
// keys are always present (empty strings as "", nil any-values as null); the
// four hook-rewrite keys appear iff their stage rewrote — including the
// hookRewriteCount:0 edge when a rewrite left the count at zero, which the old
// map emitted unconditionally inside the rewrite branch.
func TestBuildDetails_WireShape(t *testing.T) {
	base := map[string]any{
		"requestId":              "r1",
		"clientRequestId":        "cr1",
		"sourceApp":              "app",
		"cacheKey":               "ck",
		"responseHookReason":     "rhr",
		"responseHookReasonCode": "rhrc",
		"routingDecision":        nil,
		"qualitySignals":         nil,
		"complianceFlags":        nil,
		"metadata":               nil,
	}
	rec := Record{
		RequestID: "r1", ClientRequestID: "cr1", SourceApp: "app", CacheKey: "ck",
		ResponseHookReason: "rhr", ResponseHookReasonCode: "rhrc",
	}

	t.Run("no_rewrite_omits_all_hook_keys", func(t *testing.T) {
		got := jsonbMap(t, buildDetails(&rec))
		if !reflect.DeepEqual(got, base) {
			t.Errorf("details JSONB mismatch\n got: %#v\nwant: %#v", got, base)
		}
	})

	t.Run("request_rewrite_with_zero_count_still_emits_count", func(t *testing.T) {
		r := rec
		r.HookRewritten = true
		r.HookRewriteCount = 0
		got := jsonbMap(t, buildDetails(&r))
		if got["hookRewritten"] != true {
			t.Errorf("hookRewritten = %v, want true", got["hookRewritten"])
		}
		// Present-with-zero is the contract: pointer + omitempty emits 0, never omits.
		if c, ok := got["hookRewriteCount"]; !ok || c != float64(0) {
			t.Errorf("hookRewriteCount = %v (present=%v), want 0 present", c, ok)
		}
	})

	t.Run("both_stages_rewrite_emit_all_four_keys", func(t *testing.T) {
		r := rec
		r.HookRewritten = true
		r.HookRewriteCount = 3
		r.ResponseHookRewritten = true
		r.ResponseHookRewriteCount = 2
		got := jsonbMap(t, buildDetails(&r))
		if got["hookRewritten"] != true || got["hookRewriteCount"] != float64(3) {
			t.Errorf("request rewrite keys wrong: %v / %v", got["hookRewritten"], got["hookRewriteCount"])
		}
		if got["responseHookRewritten"] != true || got["responseHookRewriteCount"] != float64(2) {
			t.Errorf("response rewrite keys wrong: %v / %v", got["responseHookRewritten"], got["responseHookRewriteCount"])
		}
	})
}
