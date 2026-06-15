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
	GetByEmail(ctx context.Context, email string) (userID string, pwdHash string, source string, disabledAt *time.Time, err error)
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

	uid, hash, source, disabledAt, err := l.users.GetByEmail(ctx, email)
	if err != nil {
		// Burn time against the dummy so missing users take as long as present
		// ones. Ignore the boolean result.
		_ = auth.VerifyPassword(password, dummyHash)
		return nil, ErrInvalidCredentials
	}
	if source != "local" {
		// Federated (oidc/scim) accounts NEVER authenticate via a local
		// password — even if a stale passwordHash lingers on the row from a
		// prior local life or a mis-provisioning. Fail closed with the generic
		// invalid-credentials (after burning time) so an anonymous caller
		// cannot tell an SSO account apart from a wrong password; real SSO
		// users are guided by the per-provider buttons the login page renders.
		_ = auth.VerifyPassword(password, dummyHash)
		return nil, ErrInvalidCredentials
	}
	if hash == "" {
		// User exists but has no local password (e.g. SSO-only account). Return
		// the generic invalid-credentials so an anonymous caller cannot tell an
		// SSO-only account apart from a wrong password — the login page already
		// renders each provider's SSO button to guide real users. Burn time
		// against the dummy to keep the wrong-password path uniform.
		_ = auth.VerifyPassword(password, dummyHash)
		return nil, ErrInvalidCredentials
	}
	if disabledAt != nil {
		// A disabled local account returns the SAME generic invalid-credentials
		// error as a wrong password / SSO-only / nonexistent account so an
		// anonymous caller cannot enumerate disabled accounts. Burning
		// time against the dummy keeps the timing profile uniform. A genuinely
		// disabled user who knows their password is guided by support out of
		// band; the login page never reveals account state to anonymous callers.
		_ = auth.VerifyPassword(password, dummyHash)
		return nil, ErrInvalidCredentials
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
