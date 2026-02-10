package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"golang.org/x/crypto/argon2"

	"github.com/Madhav-000-s/ledgerd/internal/platform/id"
	"github.com/Madhav-000-s/ledgerd/internal/platform/log"
)

// Role is what an API key is permitted to do.
type Role string

const (
	RoleRead  Role = "read"
	RoleWrite Role = "write"
	RoleAdmin Role = "admin"
)

// rank orders roles so that a handler can require a minimum rather than enumerate.
func (r Role) rank() int {
	switch r {
	case RoleRead:
		return 1
	case RoleWrite:
		return 2
	case RoleAdmin:
		return 3
	default:
		return 0
	}
}

// AtLeast reports whether r is permitted to do what want requires.
func (r Role) AtLeast(want Role) bool { return r.rank() >= want.rank() }

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r.rank() > 0 }

// Key prefixes. The mode is visible in the key itself so that a test key pasted into a
// production config is obvious on sight rather than after the first charge.
const (
	prefixLive = "sk_live_"
	prefixTest = "sk_test_"

	// lookupLen is how much of the key is stored in the clear. It is not secret on its
	// own, and it turns verification into one index seek plus one hash — rather than
	// running argon2id, which is deliberately slow, once per stored key.
	lookupLen = 8
	// secretLen is the length of the random component, in base32 characters. 32
	// characters of a 32-symbol alphabet is 160 bits.
	secretLen = 32
)

// Auth is the authenticated caller.
type Auth struct {
	KeyID      string
	MerchantID string
	Role       Role
	Livemode   bool
}

type ctxKeyAuth struct{}

// AuthFrom returns the authenticated caller carried by ctx.
func AuthFrom(ctx context.Context) (Auth, bool) {
	a, ok := ctx.Value(ctxKeyAuth{}).(Auth)
	return a, ok
}

// APIKeyRecord is a stored key, without the secret.
type APIKeyRecord struct {
	ID         string
	MerchantID string
	Lookup     string
	SecretHash []byte
	Role       Role
	Livemode   bool
	Revoked    bool
}

// APIKeyStore resolves the non-secret lookup prefix to a stored key.
//
// Declared here and implemented by the postgres package, so the HTTP layer can be tested
// without a database.
type APIKeyStore interface {
	// FindByLookup returns the key with this lookup prefix, or ErrAPIKeyNotFound.
	FindByLookup(ctx context.Context, lookup string) (*APIKeyRecord, error)
}

// ErrAPIKeyNotFound means no key has that lookup prefix.
var ErrAPIKeyNotFound = errors.New("httpapi: api key not found")

// GenerateAPIKey produces a new secret and the values to store for it.
//
// The secret is returned exactly once, here. Nothing persists it, and it cannot be
// recovered from the stored hash — a caller who loses it rotates rather than retrieves.
func GenerateAPIKey(merchantID string, role Role, livemode bool) (secret string, rec *APIKeyRecord, err error) {
	if !role.Valid() {
		return "", nil, fmt.Errorf("httpapi: invalid role %q", role)
	}

	prefix := prefixTest
	if livemode {
		prefix = prefixLive
	}

	body, err := randomBase32(secretLen)
	if err != nil {
		return "", nil, err
	}
	secret = prefix + body

	hash, err := hashSecret(secret)
	if err != nil {
		return "", nil, err
	}

	return secret, &APIKeyRecord{
		ID:         id.New(id.PrefixAPIKey),
		MerchantID: merchantID,
		Lookup:     lookupOf(secret),
		SecretHash: hash,
		Role:       role,
		Livemode:   livemode,
	}, nil
}

// lookupOf returns the non-secret portion used to find the stored record: the prefix
// plus the first lookupLen characters of the random body.
func lookupOf(secret string) string {
	for _, p := range []string{prefixLive, prefixTest} {
		if strings.HasPrefix(secret, p) {
			body := secret[len(p):]
			if len(body) < lookupLen {
				return secret
			}
			return p + body[:lookupLen]
		}
	}
	if len(secret) < lookupLen {
		return secret
	}
	return secret[:lookupLen]
}

// argon2id parameters. Deliberately expensive: this runs once per request, and the cost
// is what makes an leaked database of hashes not a database of keys.
const (
	argonTime    = 1
	argonMemory  = 64 * 1024 // 64 MiB
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

// hashSecret returns salt||hash, so the salt travels with the digest and no second
// column can fall out of sync with it.
func hashSecret(secret string) ([]byte, error) {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		return nil, fmt.Errorf("httpapi: generating salt: %w", err)
	}

	digest := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	out := make([]byte, 0, len(salt)+len(digest))
	out = append(out, salt...)
	out = append(out, digest...)
	return out, nil
}

// verifySecret compares a presented key against a stored salt||hash in constant time.
func verifySecret(secret string, stored []byte) bool {
	if len(stored) != argonSaltLen+argonKeyLen {
		return false
	}

	salt := stored[:argonSaltLen]
	want := stored[argonSaltLen:]
	got := argon2.IDKey([]byte(secret), salt, argonTime, argonMemory, argonThreads, argonKeyLen)

	// Constant time: a timing-variable comparison leaks the digest a byte at a time.
	return subtle.ConstantTimeCompare(got, want) == 1
}

// randomBase32 returns n characters from the Crockford alphabet, drawn without modulo
// bias by rejecting the tail of the byte range.
func randomBase32(n int) (string, error) {
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	const limit = 256 - (256 % len(alphabet)) // reject bytes at or above this

	out := make([]byte, 0, n)
	buf := make([]byte, n)

	for len(out) < n {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("httpapi: generating key: %w", err)
		}
		for _, b := range buf {
			if int(b) >= limit {
				continue
			}
			out = append(out, alphabet[int(b)%len(alphabet)])
			if len(out) == n {
				break
			}
		}
	}
	return string(out), nil
}

// bearerToken extracts the key from the request.
//
// Both Authorization: Bearer and HTTP basic auth with the key as the username are
// accepted, because curl users reach for -u and every payments API they have used
// supports it.
func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" {
		return ""
	}

	if token, ok := strings.CutPrefix(header, "Bearer "); ok {
		return strings.TrimSpace(token)
	}
	if user, _, ok := r.BasicAuth(); ok {
		return user
	}
	return ""
}

// authenticate resolves the API key on every request.
//
// A failure is always the same response regardless of cause: an unknown key, a revoked
// key and a wrong secret are indistinguishable to a caller, so the endpoint cannot be
// used to enumerate which keys exist.
func authenticate(store APIKeyStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			secret := bearerToken(r)
			if secret == "" {
				writeError(w, r, errUnauthorized)
				return
			}

			rec, err := store.FindByLookup(r.Context(), lookupOf(secret))
			if err != nil || rec == nil || rec.Revoked || !verifySecret(secret, rec.SecretHash) {
				if err != nil && !errors.Is(err, ErrAPIKeyNotFound) {
					// A store failure is a server problem, not a client one, and must
					// not be reported as an authentication failure.
					writeError(w, r, err)
					return
				}
				writeError(w, r, errUnauthorized)
				return
			}

			auth := Auth{
				KeyID:      rec.ID,
				MerchantID: rec.MerchantID,
				Role:       rec.Role,
				Livemode:   rec.Livemode,
			}

			ctx := context.WithValue(r.Context(), ctxKeyAuth{}, auth)
			ctx = log.With(ctx, slog.String(log.KeyMerchantID, auth.MerchantID))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// requireRole rejects a caller whose key is not permitted to perform the action.
func requireRole(want Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auth, ok := AuthFrom(r.Context())
			if !ok {
				writeError(w, r, errUnauthorized)
				return
			}
			if !auth.Role.AtLeast(want) {
				writeError(w, r, errForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
