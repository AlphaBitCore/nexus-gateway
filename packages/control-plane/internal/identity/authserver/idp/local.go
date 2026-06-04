package idp

import (
	"context"
	"crypto/rand"
	"strings"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/control-plane/internal/identity/authn"
)

// UserLookup is the subset of the NexusUser store the Local adapter needs.
// The signature matches store.UserStore.GetByEmail exactly so production
// code can pass the real store and tests can pass a fake.
type UserLookup interface {
	GetByEmail(ctx context.Context, email string) (userID string, pwdHash string, disabledAt *time.Time, err error)
}

// Local is the password-based adapter backed by NexusUser.passwordHash.
// Password verification uses scrypt via auth.VerifyPassword, matching the
// existing hash format stored by the Control Plane.
type Local struct {
	users UserLookup
	idpID string
}

// dummyHash is used to keep Authenticate's elapsed time roughly constant across
// user-exists / user-missing / SSO-only outcomes, preventing scrypt-timing
// user-enumeration. Computed once at init time against a random password.
var dummyHash = mustDummyHash()

// randReadFn and hashPasswordFn are indirected through package-level vars so
// unit tests can exercise mustDummyHash's defensive panic branches without
// monkey-patching the standard library. Defaults point at crypto/rand.Read
// and auth.HashPassword; production callers MUST NOT reassign these. Mirrors
// the pwRandRead / pwScryptKey seam already in auth/password.go.
var (
	randReadFn     = rand.Read
	hashPasswordFn = auth.HashPassword
)

// mustDummyHash builds dummyHash at init. The two panic branches are
// defensive but unreachable on Go ≥1.26 through the default function values:
//   - rand.Read failures fatal the process (go.dev/issue/66821) rather
//     than returning err.
//   - auth.HashPassword only errors on bcrypt parameter-validation,
//     not possible with the fixed 32-byte input here.
//
// Kept as guards; tests drive both branches by swapping randReadFn /
// hashPasswordFn (see local_extra_test.go).
func mustDummyHash() string {
	b := make([]byte, 32)
	if _, err := randReadFn(b); err != nil {
		panic("idp: init dummyHash: " + err.Error())
	}
	h, err := hashPasswordFn(string(b))
	if err != nil {
		panic("idp: init dummyHash: " + err.Error())
	}
	return h
}

// NewLocal returns a Local adapter. idpID is the IdentityProvider.id UUID
// of the local provider row; callers resolve it once at startup from
// IdPStore.GetLocal.
func NewLocal(users UserLookup, idpID string) *Local {
	return &Local{users: users, idpID: idpID}
}

// Authenticate consumes input["email"] and input["password"]. The email is
// lowercased and trimmed before lookup so callers need not normalize
// themselves. Any miss in the store (including ErrUserNotFound) is mapped
// to ErrInvalidCredentials so the response does not leak user existence,
// and the not-found / empty-hash / disabled paths still run scrypt against
// a dummy hash to keep elapsed time roughly constant and block timing-based
// user enumeration.
func (l *Local) Authenticate(ctx context.Context, input map[string]string) (*AuthResult, error) {
	email := strings.ToLower(strings.TrimSpace(input["email"]))
	password := input["password"]
	if email == "" || password == "" {
		return nil, ErrInvalidCredentials
	}

	uid, hash, disabledAt, err := l.users.GetByEmail(ctx, email)
	if err != nil {
		// Burn time against the dummy so missing users take as long as present
		// ones. Ignore the boolean result.
		_ = auth.VerifyPassword(password, dummyHash)
		return nil, ErrInvalidCredentials
	}
	if hash == "" {
		// User exists but has no local password (e.g. SSO-only account).
		_ = auth.VerifyPassword(password, dummyHash)
		return nil, ErrInvalidCredentials
	}
	if disabledAt != nil {
		// Disabled-user enumeration is a weaker threat (the attacker already
		// guessed the right email), but burning time keeps the timing profile
		// uniform across all failure paths.
		_ = auth.VerifyPassword(password, dummyHash)
		return nil, ErrUserDisabled
	}
	if !auth.VerifyPassword(password, hash) {
		return nil, ErrInvalidCredentials
	}

	return &AuthResult{
		UserID: uid,
		IdPID:  l.idpID,
		Email:  email,
		AMR:    []string{"pwd"},
	}, nil
}
