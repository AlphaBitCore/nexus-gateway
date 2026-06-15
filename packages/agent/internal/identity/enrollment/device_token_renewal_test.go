package enrollment

// device_token_renewal_test.go covers the agent-side device-token rotation
// lifecycle (F-0202): the renewal-window check, the expiry read, and the
// rotate-and-persist path that invalidates the prior token.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/agent/internal/sync/hub"
)

// stubTokenRenewer is a TokenRenewer test double.
type stubTokenRenewer struct {
	resp   *hub.RenewTokenResponse
	err    error
	called int
}

func (s *stubTokenRenewer) RenewDeviceToken(_ context.Context) (*hub.RenewTokenResponse, error) {
	s.called++
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

func TestDeviceTokenExpiry_HappyAndMissing(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)

	if _, err := mgr.DeviceTokenExpiry(); err == nil {
		t.Fatal("expected error when expiry file is absent")
	}

	want := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(dir, "device-token-expires"), []byte(want.Format(time.RFC3339)), 0600); err != nil {
		t.Fatalf("seed expiry: %v", err)
	}
	got, err := mgr.DeviceTokenExpiry()
	if err != nil {
		t.Fatalf("DeviceTokenExpiry: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// Unparseable expiry surfaces an error.
	_ = os.WriteFile(filepath.Join(dir, "device-token-expires"), []byte("garbage"), 0600)
	if _, err := mgr.DeviceTokenExpiry(); err == nil {
		t.Fatal("expected parse error for malformed expiry")
	}
}

func TestDeviceTokenNeedsRenewal(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	now := time.Date(2026, 6, 7, 0, 0, 0, 0, time.UTC)

	// Missing expiry → renew (fail toward rotation so a legacy/unbounded token
	// is replaced rather than run forever).
	if !mgr.DeviceTokenNeedsRenewal(now) {
		t.Error("missing expiry must report needs-renewal")
	}

	// Fresh token: expiry well beyond the renewal window → no renewal.
	fresh := now.Add(DeviceTokenRenewWindow + 48*time.Hour)
	_ = os.WriteFile(filepath.Join(dir, "device-token-expires"), []byte(fresh.Format(time.RFC3339)), 0600)
	if mgr.DeviceTokenNeedsRenewal(now) {
		t.Errorf("token with %v left must not need renewal", time.Until(fresh))
	}

	// Near expiry: inside the renewal window → renew.
	near := now.Add(DeviceTokenRenewWindow - time.Hour)
	_ = os.WriteFile(filepath.Join(dir, "device-token-expires"), []byte(near.Format(time.RFC3339)), 0600)
	if !mgr.DeviceTokenNeedsRenewal(now) {
		t.Error("token inside the renewal window must need renewal")
	}

	// Already expired → renew.
	past := now.Add(-time.Hour)
	_ = os.WriteFile(filepath.Join(dir, "device-token-expires"), []byte(past.Format(time.RFC3339)), 0600)
	if !mgr.DeviceTokenNeedsRenewal(now) {
		t.Error("expired token must need renewal")
	}
}

func TestPersistEnrollment_WritesDeviceTokenExpiry(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	exp := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	resp := &HubEnrollResponse{
		ID:                   "agent-x",
		DeviceToken:          "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		DeviceTokenExpiresAt: exp,
	}
	if err := mgr.PersistEnrollment(resp, []byte("key"), []byte("-----BEGIN CERTIFICATE-----\nX\n-----END CERTIFICATE-----"), nil); err != nil {
		t.Fatalf("PersistEnrollment: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "device-token-expires"))
	if err != nil {
		t.Fatalf("expiry file not written: %v", err)
	}
	if string(got) != exp {
		t.Errorf("expiry = %q, want %q", string(got), exp)
	}
}

func TestRenewDeviceToken_NoRenewer(t *testing.T) {
	mgr := NewManager(t.TempDir())
	if err := mgr.RenewDeviceToken(context.Background()); err == nil {
		t.Fatal("expected error when token renewer is nil")
	}
}

func TestRenewDeviceToken_HubError(t *testing.T) {
	dir := t.TempDir()
	// Seed an existing token to prove a failed renewal does not clobber it.
	const old = "OLDTOKEN"
	_ = os.WriteFile(filepath.Join(dir, "device-token"), []byte(old), 0600)
	mgr := NewManager(dir, WithTokenRenewer(&stubTokenRenewer{err: errors.New("hub busy")}))

	if err := mgr.RenewDeviceToken(context.Background()); err == nil {
		t.Fatal("expected error when hub renewal fails")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "device-token"))
	if string(data) != old {
		t.Errorf("failed renewal must leave the old token intact, got %q", string(data))
	}
}

func TestRenewDeviceToken_EmptyTokenRejected(t *testing.T) {
	mgr := NewManager(t.TempDir(), WithTokenRenewer(&stubTokenRenewer{resp: &hub.RenewTokenResponse{DeviceToken: ""}}))
	if err := mgr.RenewDeviceToken(context.Background()); err == nil {
		t.Fatal("expected error when hub returns an empty token")
	}
}

func TestRenewDeviceToken_TokenWriteFailureWrapped(t *testing.T) {
	// The token-file write fails (createTempFn returns a file whose Write errors).
	prev := createTempFn
	createTempFn = func(_, _ string) (osFile, error) {
		return &fakeFile{nameVal: "x.tmp", writeErr: errors.New("disk full")}, nil
	}
	defer func() { createTempFn = prev }()

	mgr := NewManager(t.TempDir(), WithTokenRenewer(&stubTokenRenewer{resp: &hub.RenewTokenResponse{
		DeviceToken: "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	}}))
	err := mgr.RenewDeviceToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "write rotated device token") {
		t.Fatalf("expected wrapped token-write error, got %v", err)
	}
}

func TestRenewDeviceToken_ExpiryWriteFailureWrapped(t *testing.T) {
	// First CreateTemp (the token file) succeeds with a real temp; the second
	// (the expiry file) returns a file whose Write errors.
	prev := createTempFn
	calls := 0
	createTempFn = func(dir, pattern string) (osFile, error) {
		calls++
		if calls >= 2 {
			return &fakeFile{nameVal: filepath.Join(dir, "x.tmp"), writeErr: errors.New("disk full")}, nil
		}
		return os.CreateTemp(dir, pattern)
	}
	defer func() { createTempFn = prev }()

	mgr := NewManager(t.TempDir(), WithTokenRenewer(&stubTokenRenewer{resp: &hub.RenewTokenResponse{
		DeviceToken:          "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		DeviceTokenExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339),
	}}))
	err := mgr.RenewDeviceToken(context.Background())
	if err == nil || !strings.Contains(err.Error(), "write rotated device token expiry") {
		t.Fatalf("expected wrapped expiry-write error, got %v", err)
	}
}

func TestRenewDeviceToken_RotatesAndPersists(t *testing.T) {
	dir := t.TempDir()
	// Seed the prior token + a soon-to-lapse expiry.
	const old = "0000000000000000000000000000000000000000000000000000000000000000"
	_ = os.WriteFile(filepath.Join(dir, "device-token"), []byte(old), 0600)
	_ = os.WriteFile(filepath.Join(dir, "device-token-expires"), []byte(time.Now().Add(time.Hour).Format(time.RFC3339)), 0600)

	const newTok = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	newExp := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	renewer := &stubTokenRenewer{resp: &hub.RenewTokenResponse{DeviceToken: newTok, DeviceTokenExpiresAt: newExp}}
	mgr := NewManager(dir, WithTokenRenewer(renewer))

	if err := mgr.RenewDeviceToken(context.Background()); err != nil {
		t.Fatalf("RenewDeviceToken: %v", err)
	}
	if renewer.called != 1 {
		t.Errorf("renewer called %d times, want 1", renewer.called)
	}

	// The on-disk token must be the rotated one — the old token is gone, so a
	// copy of the old token is now useless for Hub auth (Hub also invalidated
	// its hash). This is the bounded-lifetime guarantee.
	gotTok, _ := os.ReadFile(filepath.Join(dir, "device-token"))
	if string(gotTok) != newTok {
		t.Errorf("token not rotated on disk: got %q, want %q", string(gotTok), newTok)
	}
	if string(gotTok) == old {
		t.Error("old token must be overwritten")
	}
	gotExp, _ := os.ReadFile(filepath.Join(dir, "device-token-expires"))
	if string(gotExp) != newExp {
		t.Errorf("expiry not updated: got %q, want %q", string(gotExp), newExp)
	}

	// After rotation the token is no longer in the renewal window.
	if mgr.DeviceTokenNeedsRenewal(time.Now()) {
		t.Error("freshly rotated token must not need renewal")
	}
}
