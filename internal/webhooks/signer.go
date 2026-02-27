package webhooks

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/platform/id"
	"github.com/Madhav-000-s/ledgerd/pkg/webhookverify"
)

// secretBytes is the length of a generated endpoint secret.
const secretBytes = 32

// Errors from the secret machinery.
var (
	ErrBadSecretKey = errors.New("webhooks: encryption key must be 32 bytes, hex encoded")
	ErrDecrypt      = errors.New("webhooks: could not decrypt an endpoint secret")
)

// Cipher encrypts endpoint secrets at rest.
//
// The worker has to sign with the secret, so unlike an API key it cannot be stored as a
// one-way hash. AES-GCM with a key from the environment is the MVP answer; the hash is
// stored alongside so a secret a merchant presents can still be checked without
// decrypting anything.
type Cipher struct {
	aead cipher.AEAD
}

// NewCipher builds a Cipher from a hex-encoded 32-byte key.
func NewCipher(hexKey string) (*Cipher, error) {
	key, err := hex.DecodeString(hexKey)
	if err != nil || len(key) != 32 {
		return nil, ErrBadSecretKey
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("webhooks: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("webhooks: %w", err)
	}
	return &Cipher{aead: aead}, nil
}

// Encrypt seals a secret. The nonce is prepended, so the ciphertext is self-contained
// and no second column can drift out of step with it.
func (c *Cipher) Encrypt(plaintext string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("webhooks: generating nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// Decrypt opens a sealed secret.
func (c *Cipher) Decrypt(sealed []byte) (string, error) {
	size := c.aead.NonceSize()
	if len(sealed) < size {
		return "", ErrDecrypt
	}

	plaintext, err := c.aead.Open(nil, sealed[:size], sealed[size:], nil)
	if err != nil {
		return "", ErrDecrypt
	}
	return string(plaintext), nil
}

// GenerateSecret returns a new endpoint secret with its hash and ciphertext.
//
// The plaintext is returned exactly once, here, and is shown to the merchant once at
// creation. A merchant who loses it rotates rather than retrieves.
func GenerateSecret(endpointID string, c *Cipher) (plaintext string, secret *Secret, hash, encrypted []byte, err error) {
	raw := make([]byte, secretBytes)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", nil, nil, nil, fmt.Errorf("webhooks: generating secret: %w", err)
	}
	plaintext = "whsec_" + hex.EncodeToString(raw)

	sum := sha256.Sum256([]byte(plaintext))
	hash = sum[:]

	if encrypted, err = c.Encrypt(plaintext); err != nil {
		return "", nil, nil, nil, err
	}

	return plaintext, &Secret{
		ID:         id.New(id.PrefixSecret),
		EndpointID: endpointID,
		Plaintext:  plaintext,
		Active:     true,
		CreatedAt:  time.Now().UTC(),
	}, hash, encrypted, nil
}

// Signer produces the signature header for a payload.
type Signer struct {
	now func() time.Time
}

// NewSigner returns a Signer.
func NewSigner(now func() time.Time) *Signer {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Signer{now: now}
}

// Sign returns the header value for a body under every active secret.
//
// One v1 signature per secret is what makes rotation zero-downtime: during the overlap a
// consumer verifies with whichever of the two it holds, and neither side has to
// coordinate a cutover instant.
//
// The body must be the exact bytes that will be sent. Signing a value and then
// re-serializing it before sending produces a signature for a document the consumer
// never receives.
func (s *Signer) Sign(body []byte, secrets []*Secret) (header string, timestamp time.Time, err error) {
	if len(secrets) == 0 {
		return "", time.Time{}, ErrSecretNotFound
	}

	timestamp = s.now()
	signatures := make([][]byte, 0, len(secrets))
	for _, secret := range secrets {
		if secret.Plaintext == "" {
			continue
		}
		signatures = append(signatures, webhookverify.Compute(secret.Plaintext, timestamp, body))
	}
	if len(signatures) == 0 {
		return "", time.Time{}, ErrSecretNotFound
	}

	return webhookverify.Format(timestamp, signatures...), timestamp, nil
}
