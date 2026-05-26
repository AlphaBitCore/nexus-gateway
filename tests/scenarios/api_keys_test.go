// IAM family extension (S-115 — personal API key self-service from
// catalog §5.13 gap). The /api/my/api-keys/* surface lets a user
// mint long-lived bearer tokens scoped to their own identity. Every
// step in this lifecycle has a PM-grade silent-failure mode the
// scenario must catch:
//
//   - Create returns rawKey ONCE. Subsequent list responses must not
//     leak it (zero-knowledge contract — losing this means an admin
//     reading the page sees every key in plaintext).
//   - The created key actually authenticates the user against admin
//     routes via x-admin-key (proves DB hash + lookup path is wired).
//   - Regenerate invalidates the old hash AND mints a new working key.
//   - Delete invalidates the key permanently.
//
// Cross-service: CP-only. The audit log is also asserted to catch
// silent-drop regressions in the create/regenerate/delete audit hooks.
package scenarios_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/tests/scenarios/helpers"
)

// TestS115_UserAPIKeyLifecycle — PM-grade e2e.
//
// BRAINSTORM (pre): the user-self-service API key flow has four
// transitions (create → use → regenerate → delete) and one
// zero-knowledge contract (list never re-exposes raw). All five must
// hold simultaneously or the operator-trust model breaks. The most
// expensive silent failure: the create response *does* return rawKey
// but the DB hash store is broken (FindByKeyHash returns nil even
// for valid keys), so all subsequent `x-admin-key: nxk_…` calls 401.
// A 200-status smoke on the POST alone would miss that completely.
//
// Cross-service: CP admin API for both the management path
// (POST/GET/DELETE /api/my/api-keys) AND the use path (any
// /api/admin/* endpoint via x-admin-key auth). Audit rows on
// AdminAuditLog verify the side-channel write isn't silently dropped.
//
// Assertions:
//   1. POST create returns 201 with id + rawKey + keyPrefix.
//   2. GET list contains the new key with the same keyPrefix and
//      NO raw key leak (the zero-knowledge contract).
//   3. The rawKey authenticates against GET /api/me via x-admin-key.
//   4. POST regenerate returns a new rawKey; the OLD rawKey now 401s.
//   5. The NEW rawKey authenticates against GET /api/me.
//   6. DELETE removes the key; the new rawKey now 401s too.
//   7. AdminAuditLog records create + update (regenerate) + delete
//      rows for the user's keyId within the test window.
func TestS115_UserAPIKeyLifecycle(t *testing.T) {
	sc := setupScenarioNoVK(t)
	ctx := context.Background()

	token, err := helpers.CPLogin(ctx, sc.Env)
	if err != nil {
		t.Fatalf("CPLogin: %v", err)
	}

	// (1) Create
	name := fmt.Sprintf("s115-%d", time.Now().UnixNano())
	createBody, _ := json.Marshal(map[string]any{"name": name})
	status, body, err := helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/my/api-keys", createBody)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if status != http.StatusCreated {
		t.Fatalf("create: status %d body=%q", status, truncate(body, 200))
	}
	var created struct {
		ID        string `json:"id"`
		Name      string `json:"name"`
		Key       string `json:"key"`
		KeyPrefix string `json:"keyPrefix"`
	}
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v body=%q", err, truncate(body, 300))
	}
	if created.ID == "" || created.Key == "" || created.KeyPrefix == "" {
		t.Fatalf("create response missing fields: %+v", created)
	}
	if len(created.Key) < 12 || created.Key[:4] != "nxk_" {
		t.Fatalf("create.key has wrong shape: %q", created.Key)
	}
	keyToDelete := created.ID
	sc.Cleanup.Register("delete user api key "+keyToDelete, func() error {
		_, _, _ = helpers.CPDoJSON(context.Background(), sc.Env, token,
			http.MethodDelete, "/api/my/api-keys/"+keyToDelete, nil)
		return nil
	})

	// (2) Zero-knowledge list.
	status, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodGet, "/api/my/api-keys", nil)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("list: status %d body=%q", status, truncate(body, 200))
	}
	var listResp struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode list: %v body=%q", err, truncate(body, 300))
	}
	found := false
	for _, k := range listResp.Data {
		if k["id"] == created.ID {
			found = true
			if k["keyPrefix"] != created.KeyPrefix {
				t.Errorf("list keyPrefix=%v, want %q", k["keyPrefix"], created.KeyPrefix)
			}
			if _, leak := k["key"]; leak {
				t.Errorf("ZERO-KNOWLEDGE VIOLATION: list returned 'key' field for an existing record (%+v)", k)
			}
		}
	}
	if !found {
		t.Errorf("created key id=%s not in list response", created.ID)
	}

	// (3) The raw key authenticates against /api/me.
	meStatus, meBody, err := helpers.CPDoWithKey(ctx, sc.Env, created.Key,
		http.MethodGet, "/api/admin/me")
	if err != nil {
		t.Fatalf("/me via api-key: %v", err)
	}
	if meStatus != http.StatusOK {
		t.Fatalf("created rawKey didn't authenticate against /me: status=%d body=%q",
			meStatus, truncate(meBody, 200))
	}

	// (4) Regenerate. Old key must now 401.
	status, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodPost, "/api/my/api-keys/"+created.ID+"/regenerate", nil)
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("regenerate: status %d body=%q", status, truncate(body, 200))
	}
	var regen struct {
		Key       string `json:"key"`
		KeyPrefix string `json:"keyPrefix"`
	}
	if err := json.Unmarshal(body, &regen); err != nil {
		t.Fatalf("decode regen: %v body=%q", err, truncate(body, 300))
	}
	if regen.Key == "" || regen.Key == created.Key {
		t.Fatalf("regenerate didn't produce a NEW key: %q (was %q)", regen.Key, created.Key)
	}
	oldStatus, _, err := helpers.CPDoWithKey(ctx, sc.Env, created.Key,
		http.MethodGet, "/api/admin/me")
	if err != nil {
		t.Fatalf("/me old key: %v", err)
	}
	if oldStatus != http.StatusUnauthorized {
		t.Errorf("OLD key still authenticates after regenerate: status=%d (want 401)", oldStatus)
	}

	// (5) New key authenticates.
	newStatus, _, err := helpers.CPDoWithKey(ctx, sc.Env, regen.Key,
		http.MethodGet, "/api/admin/me")
	if err != nil {
		t.Fatalf("/me new key: %v", err)
	}
	if newStatus != http.StatusOK {
		t.Errorf("regenerated key didn't authenticate: status=%d (want 200)", newStatus)
	}

	// (6) Delete + new key now 401.
	status, body, err = helpers.CPDoJSON(ctx, sc.Env, token,
		http.MethodDelete, "/api/my/api-keys/"+created.ID, nil)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if status != http.StatusOK && status != http.StatusNoContent {
		t.Fatalf("delete: status %d body=%q", status, truncate(body, 200))
	}
	delStatus, _, err := helpers.CPDoWithKey(ctx, sc.Env, regen.Key,
		http.MethodGet, "/api/admin/me")
	if err != nil {
		t.Fatalf("/me after delete: %v", err)
	}
	if delStatus != http.StatusUnauthorized {
		t.Errorf("DELETED key still authenticates: status=%d (want 401)", delStatus)
	}

	// (7) AdminAuditLog rows.
	auditDeadline := time.Now().Add(10 * time.Second)
	var auditCounts struct{ create, update, del int }
	for time.Now().Before(auditDeadline) {
		rows, err := sc.DB.Query(ctx, `
			SELECT action FROM "AdminAuditLog"
			WHERE "timestamp" > NOW() - INTERVAL '60 seconds'
			  AND "entityId" = $1
			  AND "entityType" ILIKE '%api%key%'
		`, created.ID)
		if err == nil {
			auditCounts = struct{ create, update, del int }{}
			for rows.Next() {
				var a string
				_ = rows.Scan(&a)
				switch a {
				case "create":
					auditCounts.create++
				case "update":
					auditCounts.update++
				case "delete":
					auditCounts.del++
				}
			}
			rows.Close()
			if auditCounts.create >= 1 && auditCounts.update >= 1 && auditCounts.del >= 1 {
				break
			}
		}
		time.Sleep(1 * time.Second)
	}
	if auditCounts.create < 1 || auditCounts.update < 1 || auditCounts.del < 1 {
		t.Errorf("audit drops: create=%d update=%d delete=%d (each should be >= 1)",
			auditCounts.create, auditCounts.update, auditCounts.del)
	}

	t.Logf("S-115 OK: key %s lifecycle (create→use→regen→delete) + audit create=%d update=%d delete=%d",
		created.ID, auditCounts.create, auditCounts.update, auditCounts.del)
}
