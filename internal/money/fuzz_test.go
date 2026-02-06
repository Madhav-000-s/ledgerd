package money

import (
	"strings"
	"testing"
)

// FuzzMoneyParse asserts that ParseDecimal is total: for any input it either returns an
// error or returns an amount that formats back to an equivalent string. It must never
// panic, and it must never accept a value it cannot represent.
//
// Parsing is the boundary between untrusted bytes and the ledger, so it is the one
// place where a malformed input could otherwise become a wrong number rather than a
// rejection.
func FuzzMoneyParse(f *testing.F) {
	seeds := []string{
		"0", "1", "96.80", "96.8", "-96.80", "+96.80", ".05", "0.00",
		"100", "1.234", "  12.34  ", "",
		"-", "+", ".", "..", "1.2.3", "abc", "1e5", "0x10", "1,000.00",
		"9223372036854775807", "-9223372036854775808",
		"92233720368547758.07", "-92233720368547758.08",
		"92233720368547758.08", // one past MaxInt64 in USD minor units
		strings.Repeat("9", 40),
		"0000000000000000000000000000000000000001.00",
	}
	for _, s := range seeds {
		for _, c := range []Currency{USD, JPY, BHD} {
			f.Add(s, string(c))
		}
	}

	f.Fuzz(func(t *testing.T, s, rawCurrency string) {
		c := Currency(rawCurrency)
		if !c.Valid() {
			// An unknown currency must be rejected rather than parsed against a
			// guessed exponent.
			if _, err := ParseDecimal(s, c); err == nil {
				t.Fatalf("ParseDecimal(%q, %q) accepted an unknown currency", s, rawCurrency)
			}
			return
		}

		m, err := ParseDecimal(s, c)
		if err != nil {
			return // rejecting anything is always allowed
		}

		if m.Currency() != c {
			t.Fatalf("ParseDecimal(%q, %s) currency = %s", s, c, m.Currency())
		}

		// Anything accepted must survive a format/parse round trip unchanged. This is
		// what rules out an accepted-but-misparsed value.
		out, err := m.Format()
		if err != nil {
			t.Fatalf("Format after parsing %q = %v", s, err)
		}
		back, err := ParseDecimal(out, c)
		if err != nil {
			t.Fatalf("ParseDecimal(%q) accepted %q but rejected its own output %q: %v", s, s, out, err)
		}
		if !back.Equal(m) {
			t.Fatalf("round trip: %q -> %d -> %q -> %d", s, m.Amount(), out, back.Amount())
		}
	})
}

// FuzzAllocate asserts the sum invariant survives arbitrary totals and ratio sets, and
// that Allocate never panics on input that ValidateRatios accepts.
func FuzzAllocate(f *testing.F) {
	f.Add(int64(10000), int64(1), int64(1), int64(1))
	f.Add(int64(-10000), int64(1), int64(1), int64(1))
	f.Add(int64(1), int64(0), int64(0), int64(1))
	f.Add(int64(-9223372036854775808), int64(1), int64(2), int64(3))
	f.Add(int64(9223372036854775807), int64(1000000), int64(1), int64(1))

	f.Fuzz(func(t *testing.T, total, r0, r1, r2 int64) {
		ratios := []int64{r0, r1, r2}
		if _, err := ValidateRatios(ratios); err != nil {
			return // Allocate is documented to panic on these; the caller validates first
		}

		parts := MustNew(total, USD).Allocate(ratios)
		if len(parts) != len(ratios) {
			t.Fatalf("got %d parts, want %d", len(parts), len(ratios))
		}

		// Sum without relying on int64 addition, which is what the invariant protects.
		var sum int64
		for _, p := range parts {
			var ok bool
			if sum, ok = addInt64(sum, p.Amount()); !ok {
				t.Fatalf("Allocate(%d, %v) = %v: parts overflow when summed", total, ratios, amounts(parts))
			}
		}
		if sum != total {
			t.Fatalf("Allocate(%d, %v) = %v sums to %d", total, ratios, amounts(parts), sum)
		}
	})
}
