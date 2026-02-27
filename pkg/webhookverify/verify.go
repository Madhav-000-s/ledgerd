// Package webhookverify verifies ledgerd webhook signatures.
//
// It is published as the reference implementation so that a consumer does not have to
// reconstruct the scheme from prose. Getting signature verification subtly wrong — a
// non-constant-time comparison, a missing timestamp check, verifying a re-serialized
// body — is easy, and every one of those mistakes is silent.
//
// The scheme is deliberately Stripe-compatible, so verification code already written
// against Stripe works unmodified:
//
//	Webhook-Signature: t=1753776000,v1=5257a86...,v1=9f2b1c...
//	signed_payload    = "{t}.{raw_request_body}"
//	v1                = hex(HMAC_SHA256(endpoint_secret, signed_payload))
//
// More than one v1 appears while an endpoint's secrets are being rotated. A consumer
// accepts the payload if any of them matches its secret.
package webhookverify

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Header is the name of the signature header.
const Header = "Webhook-Signature"

// DefaultTolerance is the maximum age of a signature.
//
// Without it a signed payload is valid forever, so anyone who captures one can replay it
// indefinitely. Five minutes is wide enough to absorb ordinary clock skew between two
// servers and narrow enough that a captured request stops being useful quickly.
const DefaultTolerance = 5 * time.Minute

// Errors returned by Verify.
var (
	ErrMalformedHeader = errors.New("webhookverify: malformed signature header")
	ErrNoSignatures    = errors.New("webhookverify: header carries no v1 signature")
	ErrTooOld          = errors.New("webhookverify: timestamp is outside the tolerance window")
	ErrNoMatch         = errors.New("webhookverify: no signature matched the secret")
)

// Signature is a parsed signature header.
type Signature struct {
	Timestamp time.Time
	// V1 holds every v1 signature in the header. There is more than one while an
	// endpoint's secrets are being rotated.
	V1 [][]byte
}

// Parse reads a signature header.
func Parse(header string) (*Signature, error) {
	if header == "" {
		return nil, fmt.Errorf("%w: empty", ErrMalformedHeader)
	}

	sig := &Signature{}
	var haveTimestamp bool

	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}

		switch key {
		case "t":
			seconds, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%w: timestamp %q", ErrMalformedHeader, value)
			}
			sig.Timestamp = time.Unix(seconds, 0).UTC()
			haveTimestamp = true

		case "v1":
			raw, err := hex.DecodeString(value)
			if err != nil {
				// Skip rather than fail: an unparseable signature among several valid
				// ones must not prevent the valid ones from being checked.
				continue
			}
			sig.V1 = append(sig.V1, raw)
		}
	}

	if !haveTimestamp {
		return nil, fmt.Errorf("%w: no timestamp", ErrMalformedHeader)
	}
	if len(sig.V1) == 0 {
		return nil, ErrNoSignatures
	}
	return sig, nil
}

// SignedPayload builds the bytes that are signed.
//
// The body must be the exact bytes received. Re-serializing a decoded body before
// verifying is the most common way to get this wrong: a different key order or a
// different spacing produces a different signature, and the verification fails for
// reasons that look like a bug in the sender.
func SignedPayload(timestamp time.Time, body []byte) []byte {
	prefix := strconv.FormatInt(timestamp.Unix(), 10) + "."

	out := make([]byte, 0, len(prefix)+len(body))
	out = append(out, prefix...)
	out = append(out, body...)
	return out
}

// Compute returns the v1 signature for a payload under one secret.
func Compute(secret string, timestamp time.Time, body []byte) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(SignedPayload(timestamp, body))
	return mac.Sum(nil)
}

// Verify checks a webhook request.
//
// body must be the raw bytes received. secret is the endpoint secret. tolerance bounds
// how old the signature may be; pass zero for DefaultTolerance.
//
// The comparison is constant time. A byte-by-byte comparison that returns early leaks
// how much of the signature was correct, which is enough to forge one.
func Verify(header string, body []byte, secret string, tolerance time.Duration) error {
	return VerifyAt(header, body, secret, tolerance, time.Now())
}

// VerifyAt is Verify with an explicit clock, for tests.
func VerifyAt(header string, body []byte, secret string, tolerance time.Duration, now time.Time) error {
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}

	sig, err := Parse(header)
	if err != nil {
		return err
	}

	// Absolute difference: a timestamp far in the future is as suspicious as one far in
	// the past, and only checking one direction leaves the other open.
	drift := now.Sub(sig.Timestamp)
	if drift < 0 {
		drift = -drift
	}
	if drift > tolerance {
		return fmt.Errorf("%w: %s old, tolerance is %s", ErrTooOld, drift, tolerance)
	}

	want := Compute(secret, sig.Timestamp, body)
	for _, got := range sig.V1 {
		if hmac.Equal(got, want) {
			return nil
		}
	}
	return ErrNoMatch
}

// Format renders a signature header from a timestamp and one or more signatures. The
// sender uses it; it is exported so a consumer's tests can construct a valid header
// without reimplementing the format.
func Format(timestamp time.Time, signatures ...[]byte) string {
	var b strings.Builder
	b.WriteString("t=")
	b.WriteString(strconv.FormatInt(timestamp.Unix(), 10))

	for _, s := range signatures {
		b.WriteString(",v1=")
		b.WriteString(hex.EncodeToString(s))
	}
	return b.String()
}
