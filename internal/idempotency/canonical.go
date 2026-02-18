// Package idempotency makes a money-mutating request safe to retry.
//
// The approach is Stripe's: a client-supplied key, a fingerprint of the request it was
// used with, a persisted response, and phased execution with recovery points.
//
// The property that matters is stated in DESIGN.md §7.2. The idempotency row's
// transition to succeeded and the serialized response are written in the same database
// transaction as the ledger entries, so there is no instant at which money has moved but
// the response was not recorded. A crash at any point leaves exactly one of two states:
// nothing happened, or everything happened and the response is replayable.
package idempotency

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
)

// maxFractionDigits bounds canonical decimal rendering. Amounts in this API are integers
// in minor units, so the fractional path exists only to keep the function total for
// metadata a client might send.
const maxFractionDigits = 30

// jsonWhitespace is exactly the set RFC 8259 permits between tokens.
const jsonWhitespace = " \t\n\r"

// Fingerprint is SHA-256 over the method, the path and the canonical form of the body.
//
// Canonicalization is not cosmetic. A client that reserializes its request map in a
// different key order on retry — which any language with unordered maps does routinely —
// would otherwise get a spurious 422 on a legitimate retry. That is a real and
// infuriating failure mode, and the fix belongs here rather than in the client.
func Fingerprint(method, path string, body []byte) ([]byte, error) {
	canonical, err := CanonicalJSON(body)
	if err != nil {
		return nil, err
	}

	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{'\n'})
	h.Write([]byte(path))
	h.Write([]byte{'\n'})
	h.Write(canonical)
	return h.Sum(nil), nil
}

// CanonicalJSON returns a byte-for-byte stable rendering of a JSON document:
// object keys sorted, insignificant whitespace removed, numbers normalized so that
// 1, 1.0 and 1e0 agree.
//
// An empty body canonicalizes to empty rather than erroring, so that a POST with no body
// still fingerprints consistently.
//
// Whitespace is JSON's definition — space, tab, newline, carriage return — and not Go's.
// bytes.TrimSpace also strips \f, \v and the Unicode spaces, which would make this
// function accept bodies that the handler's own decoder rejects. Two layers disagreeing
// about what counts as a valid request is not a difference worth having.
func CanonicalJSON(body []byte) ([]byte, error) {
	if len(bytes.Trim(body, jsonWhitespace)) == 0 {
		return []byte{}, nil
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // keep the literal; float64 would round it before we could canonicalize

	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("idempotency: request body is not valid JSON: %w", err)
	}
	if dec.More() {
		return nil, fmt.Errorf("idempotency: request body contains more than one JSON value")
	}

	var buf bytes.Buffer
	if err := writeCanonical(&buf, value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeCanonical(buf *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")

	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}

	case json.Number:
		normalized, err := canonicalNumber(v.String())
		if err != nil {
			return err
		}
		buf.WriteString(normalized)

	case string:
		// Go's encoder escapes strings consistently, which is all that is needed here.
		encoded, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("idempotency: encoding string: %w", err)
		}
		buf.Write(encoded)

	case []any:
		buf.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeCanonical(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')

	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			encoded, err := json.Marshal(k)
			if err != nil {
				return fmt.Errorf("idempotency: encoding key: %w", err)
			}
			buf.Write(encoded)
			buf.WriteByte(':')
			if err := writeCanonical(buf, v[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')

	default:
		return fmt.Errorf("idempotency: unexpected JSON value of type %T", value)
	}
	return nil
}

// canonicalNumber renders a JSON number literal in one exact form, so that 100, 100.0,
// 1e2 and 1.00e2 all fingerprint identically.
//
// big.Rat is used rather than float64 because a float64 round trip would make two
// genuinely different large integers compare equal — precisely the mistake this package
// exists to avoid elsewhere.
func canonicalNumber(literal string) (string, error) {
	rat, ok := new(big.Rat).SetString(literal)
	if !ok {
		return "", fmt.Errorf("idempotency: %q is not a valid JSON number", literal)
	}

	if rat.IsInt() {
		return rat.Num().String(), nil
	}

	s := rat.FloatString(maxFractionDigits)
	s = strings.TrimRight(s, "0")
	s = strings.TrimSuffix(s, ".")
	if s == "" || s == "-" {
		return "0", nil
	}
	return s, nil
}
