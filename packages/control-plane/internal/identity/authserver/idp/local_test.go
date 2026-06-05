package idp_test

import (
	"context"
	"errors"
	"testing"
	"time"

	auth "github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authserver/idp"
)

// fakeLookup implements idp.UserLookup for unit tests. users is keyed by
// the (already-lowercased) email.
type fakeLookup struct {
	users map[string]fakeUser
}

type fakeUser struct {
	id         string
	pwdHash    string
	source     string // "local" | "oidc" | "scim"; empty defaults to "local"
	disabledAt *time.Time
}

func (f fakeLookup) GetByEmail(_ context.Context, email string) (string, string, string, *time.Time, error) {
	u, ok := f.users[email]
	if !ok {
		return "", "", "", nil, errors.New("user not found")
	}
	source := u.source
	if source == "" {
		source = "local" // mirror the NexusUser.source DB default
	}
	return u.id, u.pwdHash, source, u.disabledAt, nil
}

const localIdPID = "00000000-0000-0000-0000-000000000001"

func mustHash(t *testing.T, pw string) string {
	t.Helper()
	h, err := auth.HashPassword(pw)
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	return h
}

func TestLocal_Authenticate_Success(t *testing.T) {
	lookup := fakeLookup{users: map[string]fakeUser{
		"alice@corp.com": {id: "usr_1", pwdHash: mustHash(t, "hunter2")},
	}}
	l := idp.NewLocal(lookup, localIdPID)

	res, err := l.Authenticate(context.Background(), map[string]string{
		"email":    "alice@corp.com",
		"password": "hunter2",
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.UserID != "usr_1" {
		t.Fatalf("UserID: got %q, want usr_1", res.UserID)
	}
	if res.IdPID != localIdPID {
		t.Fatalf("IdPID: got %q, want %q", res.IdPID, localIdPID)
	}
	if res.Email != "alice@corp.com" {
		t.Fatalf("Email: got %q", res.Email)
	}
	if len(res.AMR) != 1 || res.AMR[0] != "pwd" {
		t.Fatalf("AMR: got %v, want [pwd]", res.AMR)
	}
}

func TestLocal_Authenticate_NormalizesEmail(t *testing.T) {
	lookup := fakeLookup{users: map[string]fakeUser{
		"alice@corp.com": {id: "usr_1", pwdHash: mustHash(t, "hunter2")},
	}}
	l := idp.NewLocal(lookup, localIdPID)

	// Mixed case + surrounding whitespace — adapter must normalize before lookup.
	res, err := l.Authenticate(context.Background(), map[string]string{
		"email":    "  Alice@Corp.com  ",
		"password": "hunter2",
	})
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if res.Email != "alice@corp.com" {
		t.Fatalf("Email: got %q, want lowercased+trimmed", res.Email)
	}
}

func TestLocal_Authenticate_WrongPassword(t *testing.T) {
	lookup := fakeLookup{users: map[string]fakeUser{
		"alice@corp.com": {id: "usr_1", pwdHash: mustHash(t, "hunter2")},
	}}
	l := idp.NewLocal(lookup, localIdPID)

	_, err := l.Authenticate(context.Background(), map[string]string{
		"email":    "alice@corp.com",
		"password": "wrong",
	})
	if !errors.Is(err, idp.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestLocal_Authenticate_UnknownEmail(t *testing.T) {
	lookup := fakeLookup{users: map[string]fakeUser{}}
	l := idp.NewLocal(lookup, localIdPID)

	_, err := l.Authenticate(context.Background(), map[string]string{
		"email":    "ghost@corp.com",
		"password": "whatever",
	})
	// Must be ErrInvalidCredentials, NOT a distinct "not found" — we do not
	// leak email enumeration through the response.
	if !errors.Is(err, idp.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestLocal_Authenticate_Disabled(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	lookup := fakeLookup{users: map[string]fakeUser{
		"alice@corp.com": {id: "usr_1", pwdHash: mustHash(t, "hunter2"), disabledAt: &past},
	}}
	l := idp.NewLocal(lookup, localIdPID)

	_, err := l.Authenticate(context.Background(), map[string]string{
		"email":    "alice@corp.com",
		"password": "hunter2",
	})
	if !errors.Is(err, idp.ErrUserDisabled) {
		t.Fatalf("expected ErrUserDisabled, got %v", err)
	}
}

func TestLocal_Authenticate_EmptyInput(t *testing.T) {
	lookup := fakeLookup{users: map[string]fakeUser{}}
	l := idp.NewLocal(lookup, localIdPID)

	cases := []map[string]string{
		{"email": "", "password": "hunter2"},
		{"email": "alice@corp.com", "password": ""},
		{},
	}
	for _, in := range cases {
		_, err := l.Authenticate(context.Background(), in)
		if !errors.Is(err, idp.ErrInvalidCredentials) {
			t.Fatalf("input=%v: expected ErrInvalidCredentials, got %v", in, err)
		}
	}
}

func TestLocal_Authenticate_SSOOnlyUser(t *testing.T) {
	// User row exists but has no local password — SSO-only account. Must
	// reject without panicking on empty hash.
	lookup := fakeLookup{users: map[string]fakeUser{
		"alice@corp.com": {id: "usr_1", pwdHash: ""},
	}}
	l := idp.NewLocal(lookup, localIdPID)

	_, err := l.Authenticate(context.Background(), map[string]string{
		"email":    "alice@corp.com",
		"password": "hunter2",
	})
	if !errors.Is(err, idp.ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

// TestLocal_Authenticate_FederatedWithStaleHash is the regression guard for the
// Steve case: a federated (oidc/scim) account that still carries a VALID local
// passwordHash from a prior life must NOT be allowed to log in with the correct
// password — federated accounts authenticate only through their IdP. The error
// must be the generic ErrInvalidCredentials (not a distinct SSO signal) so the
// rejection is indistinguishable from a wrong password (anti-enumeration).
func TestLocal_Authenticate_FederatedWithStaleHash(t *testing.T) {
	for _, source := range []string{"oidc", "scim"} {
		t.Run(source, func(t *testing.T) {
			lookup := fakeLookup{users: map[string]fakeUser{
				"steve@corp.com": {id: "usr_steve", pwdHash: mustHash(t, "hunter2"), source: source},
			}}
			l := idp.NewLocal(lookup, localIdPID)

			res, err := l.Authenticate(context.Background(), map[string]string{
				"email":    "steve@corp.com",
				"password": "hunter2", // the CORRECT password
			})
			if res != nil {
				t.Fatalf("federated account must not authenticate; got %+v", res)
			}
			if !errors.Is(err, idp.ErrInvalidCredentials) {
				t.Fatalf("expected generic ErrInvalidCredentials, got %v", err)
			}
		})
	}
}

func TestLocal_Authenticate_ConstantishTimingOnMiss(t *testing.T) {
	// Sanity check: a missing-user call still pays scrypt cost (i.e. is slow
	// enough that we're not shortcutting). We don't assert a tight bound —
	// scrypt is intentionally slow, so "> 1ms" is sufficient to prove we
	// exercised the KDF rather than returning immediately.
	lookup := fakeLookup{users: map[string]fakeUser{}}
	l := idp.NewLocal(lookup, localIdPID)

	start := time.Now()
	_, err := l.Authenticate(context.Background(), map[string]string{
		"email":    "nobody@example.com",
		"password": "whatever",
	})
	if !errors.Is(err, idp.ErrInvalidCredentials) {
		t.Fatalf("want ErrInvalidCredentials, got %v", err)
	}
	if elapsed := time.Since(start); elapsed < time.Millisecond {
		t.Fatalf("scrypt should have burned time; only %v elapsed", elapsed)
	}
}
