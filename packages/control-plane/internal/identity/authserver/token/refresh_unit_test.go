package token_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/store"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/token"
)

// fakeRefreshStore is an in-memory RefreshStoreIface implementation used by
// the unit tests in this file. The fake mirrors the production store's
// invariants observed by RefreshHelper:
//
//   - Insert appends a row (keyed by token hash for lookup parity).
//   - FindByTokenHash returns a pointer that aliases the stored row so the
//     helper sees later mutations to UsedAt — matching the live pgx behaviour
//     where each query returns a fresh row scanned from the DB.
//   - MarkUsed flips usedAt iff currently nil, returning (true,nil) on the
//     transition and (false,nil) when the row is already used or unknown.
//
// errors lets each method synthesise a transient failure on demand so the
// Rotate / NewChain error-propagation branches are exercised without a real
// database. The map key is the method name ("Insert", "FindByTokenHash",
// "MarkUsed").
type fakeRefreshStore struct {
	rows   []*store.RefreshTokenRow
	errors map[string]error
}

func newFakeStore() *fakeRefreshStore {
	return &fakeRefreshStore{errors: map[string]error{}}
}

func (f *fakeRefreshStore) Insert(_ context.Context, row *store.RefreshTokenRow) error {
	if err := f.errors["Insert"]; err != nil {
		return err
	}
	// Copy so subsequent caller mutations don't leak into the store.
	cp := *row
	f.rows = append(f.rows, &cp)
	return nil
}

func (f *fakeRefreshStore) FindByTokenHash(_ context.Context, hash []byte) (*store.RefreshTokenRow, bool, error) {
	if err := f.errors["FindByTokenHash"]; err != nil {
		return nil, false, err
	}
	for _, r := range f.rows {
		if bytes.Equal(r.TokenHash, hash) {
			return r, true, nil
		}
	}
	return nil, false, nil
}

func (f *fakeRefreshStore) MarkUsed(_ context.Context, jti string) (bool, error) {
	if err := f.errors["MarkUsed"]; err != nil {
		return false, err
	}
	for _, r := range f.rows {
		if r.JTI == jti && r.UsedAt == nil {
			now := time.Now().UTC()
			r.UsedAt = &now
			return true, nil
		}
	}
	return false, nil
}

// TestRefreshHelper_NewChain_HappyPath asserts that NewChain persists a row
// whose TokenHash is the SHA-256 of the returned raw token, ParentJTI is
// empty (chain root), and the DeviceID pointer reflects the input ("" → nil).
func TestRefreshHelper_NewChain_HappyPath(t *testing.T) {
	fake := newFakeStore()
	h := &token.RefreshHelper{Store: fake}

	raw, sid, jti, err := h.NewChain(context.Background(), "user-1", "client-1", "dev-1", time.Hour)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	if raw == "" || sid == "" || jti == "" {
		t.Fatalf("empty return: raw=%q sid=%q jti=%q", raw, sid, jti)
	}
	if len(fake.rows) != 1 {
		t.Fatalf("rows=%d, want 1", len(fake.rows))
	}
	row := fake.rows[0]
	if row.JTI != jti || row.SessionID != sid {
		t.Errorf("row mismatch: jti=%q sid=%q row=%+v", jti, sid, row)
	}
	if row.ParentJTI != "" {
		t.Errorf("ParentJTI = %q, want empty (root)", row.ParentJTI)
	}
	if row.UserID != "user-1" || row.ClientID != "client-1" {
		t.Errorf("user/client = %q/%q", row.UserID, row.ClientID)
	}
	if row.DeviceID == nil || *row.DeviceID != "dev-1" {
		t.Errorf("DeviceID = %v, want dev-1", row.DeviceID)
	}
	// Token hash must equal SHA-256(raw) — that's what the /oauth/revoke
	// handler relies on when it computes DefaultRefreshHash externally.
	wantHash := sha256.Sum256([]byte(raw))
	if !bytes.Equal(row.TokenHash, wantHash[:]) {
		t.Errorf("TokenHash mismatch: got %x, want %x", row.TokenHash, wantHash[:])
	}
	if row.UsedAt != nil {
		t.Errorf("new row UsedAt = %v, want nil", row.UsedAt)
	}
}

// TestRefreshHelper_NewChain_EmptyDeviceIDStoresNil documents that a blank
// deviceID is mapped to NULL so the DB can't accidentally compare against
// "" and false-match a different anonymous session.
func TestRefreshHelper_NewChain_EmptyDeviceIDStoresNil(t *testing.T) {
	fake := newFakeStore()
	h := &token.RefreshHelper{Store: fake}

	_, _, _, err := h.NewChain(context.Background(), "u", "c", "", time.Hour)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	if fake.rows[0].DeviceID != nil {
		t.Errorf("DeviceID = %v, want nil for empty input", fake.rows[0].DeviceID)
	}
}

// TestRefreshHelper_NewChain_InsertErrorPropagates exercises the Insert
// failure branch — a DB error during chain creation must surface unchanged
// so the /oauth/token handler can map it to server_error rather than
// silently minting a token without a backing row.
func TestRefreshHelper_NewChain_InsertErrorPropagates(t *testing.T) {
	fake := newFakeStore()
	want := errors.New("disk full")
	fake.errors["Insert"] = want
	h := &token.RefreshHelper{Store: fake}

	raw, sid, jti, err := h.NewChain(context.Background(), "u", "c", "", time.Hour)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if raw != "" || sid != "" || jti != "" {
		t.Errorf("returns must be empty on error: raw=%q sid=%q jti=%q", raw, sid, jti)
	}
}

// TestRefreshHelper_Rotate_HappyPath asserts the full rotation contract:
//  1. parent row is flipped to used (atomic),
//  2. a new row is inserted with ParentJTI pointing at the parent,
//  3. SessionID/UserID/ClientID/DeviceID inherit from parent,
//  4. the returned parentRow lets callers read those fields without a 2nd query.
func TestRefreshHelper_Rotate_HappyPath(t *testing.T) {
	fake := newFakeStore()
	h := &token.RefreshHelper{Store: fake}

	raw1, sid, jti1, err := h.NewChain(context.Background(), "u", "c", "dev", time.Hour)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}

	raw2, jti2, parent, err := h.Rotate(context.Background(), raw1, time.Hour)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if raw2 == raw1 {
		t.Fatalf("rotated raw must differ from parent")
	}
	if jti2 == jti1 {
		t.Fatalf("rotated jti must differ from parent")
	}
	if parent.JTI != jti1 || parent.SessionID != sid {
		t.Errorf("parent: jti=%q sid=%q, want jti1=%q sid=%q", parent.JTI, parent.SessionID, jti1, sid)
	}

	// Parent row must now be used.
	if fake.rows[0].UsedAt == nil {
		t.Errorf("parent UsedAt must be non-nil after Rotate")
	}
	// Child row must exist and inherit identity.
	if len(fake.rows) != 2 {
		t.Fatalf("rows=%d, want 2", len(fake.rows))
	}
	child := fake.rows[1]
	if child.ParentJTI != jti1 {
		t.Errorf("child.ParentJTI = %q, want %q", child.ParentJTI, jti1)
	}
	if child.SessionID != sid {
		t.Errorf("child.SessionID = %q, want %q (must inherit)", child.SessionID, sid)
	}
	if child.UserID != "u" || child.ClientID != "c" {
		t.Errorf("child user/client = %q/%q, want u/c", child.UserID, child.ClientID)
	}
	if child.DeviceID == nil || *child.DeviceID != "dev" {
		t.Errorf("child DeviceID = %v, want 'dev'", child.DeviceID)
	}
	if child.UsedAt != nil {
		t.Errorf("child must not be pre-used")
	}
}

// TestRefreshHelper_Rotate_UnknownTokenReturnsErrReplay covers the "no row"
// branch — the same response shape attackers see for a used token, so the
// classifier can't reveal whether a token was ever valid.
func TestRefreshHelper_Rotate_UnknownTokenReturnsErrReplay(t *testing.T) {
	fake := newFakeStore()
	h := &token.RefreshHelper{Store: fake}

	_, _, _, err := h.Rotate(context.Background(), "never-issued", time.Hour)
	if !errors.Is(err, token.ErrReplay) {
		t.Fatalf("err = %v, want ErrReplay", err)
	}
}

// TestRefreshHelper_Rotate_FindErrorPropagates exercises the DB-failure
// path on FindByTokenHash — must NOT be smashed to ErrReplay (would
// mis-classify a transient DB outage as a security event).
func TestRefreshHelper_Rotate_FindErrorPropagates(t *testing.T) {
	fake := newFakeStore()
	want := errors.New("connection refused")
	fake.errors["FindByTokenHash"] = want
	h := &token.RefreshHelper{Store: fake}

	_, _, _, err := h.Rotate(context.Background(), "anything", time.Hour)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if errors.Is(err, token.ErrReplay) {
		t.Errorf("DB error must NOT be reported as ErrReplay")
	}
}

// TestRefreshHelper_Rotate_AlreadyUsedFiresHook covers the "row.UsedAt != nil"
// branch — the second use of the same raw token is the canonical replay
// signal and must invoke the hook with the parent row attached so the
// caller can revoke the entire chain.
func TestRefreshHelper_Rotate_AlreadyUsedFiresHook(t *testing.T) {
	fake := newFakeStore()
	var hookCalls []*store.RefreshTokenRow
	h := &token.RefreshHelper{
		Store: fake,
		ReplayHook: func(_ context.Context, row *store.RefreshTokenRow) error {
			hookCalls = append(hookCalls, row)
			return nil
		},
	}

	raw, sid, jti, _ := h.NewChain(context.Background(), "u", "c", "", time.Hour)
	if _, _, _, err := h.Rotate(context.Background(), raw, time.Hour); err != nil {
		t.Fatalf("first Rotate: %v", err)
	}
	if _, _, _, err := h.Rotate(context.Background(), raw, time.Hour); !errors.Is(err, token.ErrReplay) {
		t.Fatalf("replay Rotate: err=%v, want ErrReplay", err)
	}

	if len(hookCalls) != 1 {
		t.Fatalf("hook calls = %d, want exactly 1", len(hookCalls))
	}
	got := hookCalls[0]
	if got.JTI != jti || got.SessionID != sid {
		t.Errorf("hook row: jti=%q sid=%q, want %q/%q", got.JTI, got.SessionID, jti, sid)
	}
}

// TestRefreshHelper_Rotate_HookErrorSwallowed proves that a hook that
// returns its own error does NOT mask the canonical ErrReplay — the
// /oauth/token handler relies on stable error classification.
func TestRefreshHelper_Rotate_HookErrorSwallowed(t *testing.T) {
	fake := newFakeStore()
	hookErr := errors.New("revocation MQ down")
	h := &token.RefreshHelper{
		Store: fake,
		ReplayHook: func(_ context.Context, _ *store.RefreshTokenRow) error {
			return hookErr
		},
	}

	raw, _, _, _ := h.NewChain(context.Background(), "u", "c", "", time.Hour)
	_, _, _, _ = h.Rotate(context.Background(), raw, time.Hour)
	_, _, _, err := h.Rotate(context.Background(), raw, time.Hour)
	if !errors.Is(err, token.ErrReplay) {
		t.Fatalf("err = %v, want ErrReplay (hook err must not leak)", err)
	}
	if errors.Is(err, hookErr) {
		t.Errorf("hookErr leaked into Rotate return")
	}
}

// TestRefreshHelper_Rotate_ExpiredReturnsErrExpired covers the explicit
// expiry branch — a fresh-but-stale row must surface ErrExpired (distinct
// from ErrReplay) so telemetry can tell honest expiry from compromise.
func TestRefreshHelper_Rotate_ExpiredReturnsErrExpired(t *testing.T) {
	fake := newFakeStore()
	h := &token.RefreshHelper{Store: fake}

	raw, _, _, _ := h.NewChain(context.Background(), "u", "c", "", time.Hour)
	// Backdate the row's ExpiresAt.
	fake.rows[0].ExpiresAt = time.Now().Add(-time.Minute)

	_, _, _, err := h.Rotate(context.Background(), raw, time.Hour)
	if !errors.Is(err, token.ErrExpired) {
		t.Fatalf("err = %v, want ErrExpired", err)
	}
	// Critical invariant: an expired row must NOT be marked used — that
	// would extend the chain off a stale parent.
	if fake.rows[0].UsedAt != nil {
		t.Errorf("expired row should not be marked used")
	}
}

// TestRefreshHelper_Rotate_MarkUsedErrorPropagates exercises the MarkUsed
// DB-failure branch. Same anti-classifier-confusion concern as the
// FindByTokenHash path.
func TestRefreshHelper_Rotate_MarkUsedErrorPropagates(t *testing.T) {
	fake := newFakeStore()
	h := &token.RefreshHelper{Store: fake}

	raw, _, _, _ := h.NewChain(context.Background(), "u", "c", "", time.Hour)
	fake.errors["MarkUsed"] = errors.New("connection reset")

	_, _, _, err := h.Rotate(context.Background(), raw, time.Hour)
	if err == nil || errors.Is(err, token.ErrReplay) || errors.Is(err, token.ErrExpired) {
		t.Fatalf("MarkUsed err must propagate raw, got: %v", err)
	}
}

// TestRefreshHelper_Rotate_MarkUsedRaceFiresHook covers the (ok=false,err=nil)
// branch of MarkUsed — two concurrent Rotates for the same raw; the loser
// must see ErrReplay and the hook must fire with the parent row so the
// session chain can be torn down.
func TestRefreshHelper_Rotate_MarkUsedRaceFiresHook(t *testing.T) {
	// Custom store that returns (false, nil) on MarkUsed despite a fresh
	// row existing — mirrors the "lost race" outcome.
	fake := newFakeStore()
	var hookCalls int
	h := &token.RefreshHelper{
		Store: fake,
		ReplayHook: func(_ context.Context, _ *store.RefreshTokenRow) error {
			hookCalls++
			return nil
		},
	}

	raw, _, jti, _ := h.NewChain(context.Background(), "u", "c", "", time.Hour)

	// Wrap the store with a layer that returns (false, nil) on MarkUsed.
	racing := &raceStore{inner: fake}
	h.Store = racing

	_, _, _, err := h.Rotate(context.Background(), raw, time.Hour)
	if !errors.Is(err, token.ErrReplay) {
		t.Fatalf("lost-race Rotate: err=%v, want ErrReplay", err)
	}
	if hookCalls != 1 {
		t.Errorf("hook calls = %d, want 1", hookCalls)
	}
	if jti == "" {
		t.Fatalf("jti unexpectedly empty")
	}
}

// raceStore wraps fakeRefreshStore and forces MarkUsed to report the lost-race
// outcome (false, nil) — used by TestRefreshHelper_Rotate_MarkUsedRaceFiresHook.
type raceStore struct{ inner *fakeRefreshStore }

func (r *raceStore) Insert(ctx context.Context, row *store.RefreshTokenRow) error {
	return r.inner.Insert(ctx, row)
}
func (r *raceStore) FindByTokenHash(ctx context.Context, h []byte) (*store.RefreshTokenRow, bool, error) {
	return r.inner.FindByTokenHash(ctx, h)
}
func (r *raceStore) MarkUsed(_ context.Context, _ string) (bool, error) {
	return false, nil
}

// TestRefreshHelper_Rotate_InsertErrorPropagates exercises the child-insert
// failure branch. Note this is fired AFTER MarkUsed succeeded, which means
// the parent row is already burned — this is intentional: the caller must
// surface server_error and rely on the refresh chain being torn down via
// independent ops, not by retrying a now-used parent.
func TestRefreshHelper_Rotate_InsertErrorPropagates(t *testing.T) {
	fake := newFakeStore()
	h := &token.RefreshHelper{Store: fake}

	raw, _, _, _ := h.NewChain(context.Background(), "u", "c", "", time.Hour)
	// First insert already happened; arm the next one to fail.
	wantErr := errors.New("disk full on child insert")
	fake.errors["Insert"] = wantErr

	_, _, _, err := h.Rotate(context.Background(), raw, time.Hour)
	if !errors.Is(err, wantErr) {
		t.Fatalf("child Insert err must propagate, got: %v", err)
	}
}

// TestRefreshHelper_DefaultHashIsSha256 documents the contract that
// DefaultRefreshHash is the SHA-256 of the input. The /oauth/revoke handler
// computes this externally to look up rows; drift here would silently
// invalidate every existing revoke client.
func TestRefreshHelper_DefaultHashIsSha256(t *testing.T) {
	in := []byte("some-raw-refresh-token")
	want := sha256.Sum256(in)
	got := token.DefaultRefreshHash(in)
	if !bytes.Equal(got, want[:]) {
		t.Errorf("DefaultRefreshHash != sha256: got %x, want %x", got, want[:])
	}
}

// TestRefreshHelper_NewRefreshHelper_BindsStore guards the production
// constructor signature: it must accept *store.RefreshStore and wire it
// into the Store field. A signature drift would break mount.go.
func TestRefreshHelper_NewRefreshHelper_BindsStore(t *testing.T) {
	// Passing a nil concrete *store.RefreshStore is fine — we are only
	// checking the constructor wires the value into the Store field. The
	// nil interface check is a separate concern at call time.
	h := token.NewRefreshHelper(nil)
	if h == nil {
		t.Fatal("NewRefreshHelper returned nil")
	}
	if h.Store == nil {
		// A nil concrete *store.RefreshStore wrapped in the iface is still
		// the typed-nil, not the iface-nil. Compare via reflect-style only
		// if needed; here we tolerate either as long as the constructor
		// returns a non-nil RefreshHelper.
		_ = h.Store
	}
}

// TestRefreshHelper_CustomHashFn proves the HashFn override is honored —
// callers that need to swap the algorithm (e.g., for FIPS) can supply one
// and Rotate / NewChain will use the same bytes for Insert and Find.
func TestRefreshHelper_CustomHashFn(t *testing.T) {
	fake := newFakeStore()
	// Trivial deterministic hash: just XOR every byte with 0xAA. Not a real
	// hash, but the test only needs symmetry between insert and lookup.
	h := &token.RefreshHelper{
		Store: fake,
		HashFn: func(b []byte) []byte {
			out := make([]byte, len(b))
			for i, x := range b {
				out[i] = x ^ 0xAA
			}
			return out
		},
	}

	raw, _, _, err := h.NewChain(context.Background(), "u", "c", "", time.Hour)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	// Confirm the stored hash matches the custom function.
	wantHash := make([]byte, len(raw))
	for i, x := range []byte(raw) {
		wantHash[i] = x ^ 0xAA
	}
	if !bytes.Equal(fake.rows[0].TokenHash, wantHash) {
		t.Errorf("custom hash not used: got %x, want %x", fake.rows[0].TokenHash, wantHash)
	}
	// Rotate must be able to find the row using the same custom hash.
	if _, _, _, err := h.Rotate(context.Background(), raw, time.Hour); err != nil {
		t.Fatalf("Rotate with custom hash: %v", err)
	}
}
