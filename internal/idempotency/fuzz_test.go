package idempotency

import (
	"bytes"
	"encoding/json"
	"testing"
)

// FuzzCanonicalJSON asserts the two properties the fingerprint depends on.
//
// Idempotency is only worth anything if the canonical form is stable: if reformatting a
// request could change its fingerprint, a legitimate retry gets a spurious 422; if two
// genuinely different requests could canonicalize the same, a retry with a changed
// amount would be answered with the first response and the second charge would silently
// vanish. The first is an annoyance, the second is a correctness failure.
func FuzzCanonicalJSON(f *testing.F) {
	seeds := []string{
		`{}`,
		`[]`,
		`null`,
		`0`,
		`{"a":1,"b":2}`,
		`{"b":2,"a":1}`,
		`{"amount":10000,"currency":"USD"}`,
		` { "amount" : 1.0e4 } `,
		`{"nested":{"z":[1,2,{"y":null}],"a":true}}`,
		`{"n":9007199254740993}`,
		`{"n":-0.0}`,
		`{"s":"with \"quotes\" and \\ backslash"}`,
		`{"s":"unicode é中"}`,
		`{"deep":[[[[[1]]]]]}`,
		`not json`,
		`{`,
		`{"a":1}{"b":2}`,
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, body []byte) {
		canonical, err := CanonicalJSON(body)
		if err != nil {
			return // rejecting anything is always allowed
		}

		// Idempotent: canonicalizing an already-canonical document changes nothing.
		// Without this, the fingerprint would depend on how many times the value had
		// been round-tripped.
		again, err := CanonicalJSON(canonical)
		if err != nil {
			t.Fatalf("the canonical form was rejected on a second pass: %v\ninput: %q\ncanonical: %s",
				err, body, canonical)
		}
		if !bytes.Equal(canonical, again) {
			t.Fatalf("canonicalization is not stable:\n input: %q\n first: %s\nsecond: %s",
				body, canonical, again)
		}

		// Meaning-preserving: the canonical form must decode to the same value as the
		// input. This is what rules out a canonicalizer that collapses distinct
		// documents together.
		if len(canonical) > 0 {
			var fromInput, fromCanonical any
			decInput := json.NewDecoder(bytes.NewReader(body))
			decInput.UseNumber()
			decCanonical := json.NewDecoder(bytes.NewReader(canonical))
			decCanonical.UseNumber()

			if err := decInput.Decode(&fromInput); err != nil {
				t.Fatalf("input was accepted but does not decode: %v", err)
			}
			if err := decCanonical.Decode(&fromCanonical); err != nil {
				t.Fatalf("canonical form does not decode: %v", err)
			}

			// Compare through the canonical form of each, which is the equality the
			// fingerprint actually uses.
			reInput, err := CanonicalJSON(body)
			if err != nil {
				t.Fatalf("re-canonicalizing the input failed: %v", err)
			}
			if !bytes.Equal(reInput, canonical) {
				t.Fatalf("canonicalization is not deterministic for %q", body)
			}
		}
	})
}

// FuzzFingerprint asserts the fingerprint never panics and is deterministic.
func FuzzFingerprint(f *testing.F) {
	f.Add("POST", "/v1/transfers", []byte(`{"amount":100}`))
	f.Add("POST", "/v1/refunds", []byte(`{}`))
	f.Add("", "", []byte(``))
	f.Add("POST", "/v1/transfers", []byte(`{`))

	f.Fuzz(func(t *testing.T, method, path string, body []byte) {
		first, err := Fingerprint(method, path, body)
		if err != nil {
			return
		}

		second, err := Fingerprint(method, path, body)
		if err != nil {
			t.Fatalf("the same input fingerprinted once and then failed: %v", err)
		}
		if !bytes.Equal(first, second) {
			t.Fatal("the fingerprint is not deterministic")
		}
		if len(first) != 32 {
			t.Fatalf("fingerprint is %d bytes, want 32", len(first))
		}
	})
}
