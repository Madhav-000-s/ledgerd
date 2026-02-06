package money

import (
	"math"
	"math/big"
	"testing"

	"pgregory.net/rapid"
)

// drawCurrency picks from the real exponent table rather than a hand-picked subset, so
// the zero- and three-decimal currencies are exercised as often as the two-decimal ones.
func drawCurrency(t *rapid.T) Currency {
	all := Currencies()
	return all[rapid.IntRange(0, len(all)-1).Draw(t, "currency_index")]
}

// P3 — Allocate always sums exactly to the total, for every total including negative
// and extreme ones, and for every valid ratio set.
//
// This is the property that justifies the absence of a Div. If it ever fails, money is
// being created or destroyed by a rounding step.
func TestPropertyAllocateSumsExactly(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		total := rapid.Int64().Draw(t, "total")
		currency := drawCurrency(t)

		n := rapid.IntRange(1, 12).Draw(t, "parts")
		ratios := make([]int64, n)
		var ratioSum int64
		for i := range ratios {
			// Bounded so the ratios cannot themselves overflow; zero ratios are
			// allowed because a split with an inactive party is legitimate.
			ratios[i] = rapid.Int64Range(0, 1_000_000).Draw(t, "ratio")
			ratioSum += ratios[i]
		}
		if ratioSum == 0 {
			t.Skip("ratios sum to zero")
		}

		parts := MustNew(total, currency).Allocate(ratios)

		if len(parts) != n {
			t.Fatalf("got %d parts, want %d", len(parts), n)
		}

		// Sum in big.Int: the parts are int64 and their true sum is the total, but
		// adding them in int64 would itself be the thing under test.
		sum := new(big.Int)
		for _, p := range parts {
			if p.Currency() != currency {
				t.Fatalf("part currency = %s, want %s", p.Currency(), currency)
			}
			sum.Add(sum, big.NewInt(p.Amount()))
		}

		if sum.Cmp(big.NewInt(total)) != 0 {
			t.Fatalf("Allocate(%d, %v) parts sum to %s, want %d", total, ratios, sum, total)
		}
	})
}

// Every part must land within one minor unit of its exact proportional share, so that
// "sums exactly" cannot be satisfied by a degenerate distribution.
func TestPropertyAllocateIsFair(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		total := rapid.Int64Range(-1<<40, 1<<40).Draw(t, "total")
		currency := drawCurrency(t)

		n := rapid.IntRange(1, 8).Draw(t, "parts")
		ratios := make([]int64, n)
		var ratioSum int64
		for i := range ratios {
			ratios[i] = rapid.Int64Range(0, 10_000).Draw(t, "ratio")
			ratioSum += ratios[i]
		}
		if ratioSum == 0 {
			t.Skip("ratios sum to zero")
		}

		parts := MustNew(total, currency).Allocate(ratios)
		bigSum := big.NewInt(ratioSum)

		for i, p := range parts {
			lhs := new(big.Int).Mul(big.NewInt(p.Amount()), bigSum)
			rhs := new(big.Int).Mul(big.NewInt(total), big.NewInt(ratios[i]))
			diff := new(big.Int).Abs(new(big.Int).Sub(lhs, rhs))

			if diff.Cmp(bigSum) >= 0 {
				t.Fatalf("Allocate(%d, %v) part %d = %d is more than one unit from its exact share",
					total, ratios, i, p.Amount())
			}
		}
	})
}

// P4 — MulBps is exact and cannot overflow for any |amount| < 2^53 at any rate a fee
// schedule would use.
//
// The interesting case is that amount*bps overflows an int64 long before the result
// does: 2^53 * 10000 is about 9.0e19, an order of magnitude past MaxInt64. An
// implementation that multiplies first produces a negative fee on a large charge.
func TestPropertyMulBpsNeverOverflows(t *testing.T) {
	const limit = int64(1) << 53

	rapid.Check(t, func(t *rapid.T) {
		amount := rapid.Int64Range(-limit+1, limit-1).Draw(t, "amount")
		bps := rapid.Int64Range(0, 10_000).Draw(t, "bps")
		currency := drawCurrency(t)

		got := MustNew(amount, currency).MulBps(bps)

		// Recompute in exact arithmetic: floor((amount*bps + 5000) / 10000).
		num := new(big.Int).Mul(big.NewInt(amount), big.NewInt(bps))
		num.Add(num, big.NewInt(halfScale))
		want := new(big.Int)
		mod := new(big.Int)
		want.DivMod(num, big.NewInt(bpsScale), mod) // Euclidean; adjust to floor below
		if mod.Sign() != 0 && num.Sign() < 0 {
			// big.Int.DivMod already floors for a positive divisor, so nothing to do;
			// this branch exists only to make the intent explicit if that changes.
			_ = mod
		}

		if want.Cmp(big.NewInt(got.Amount())) != 0 {
			t.Fatalf("Money(%d).MulBps(%d) = %d, want %s", amount, bps, got.Amount(), want)
		}
		if got.Currency() != currency {
			t.Fatalf("currency = %s, want %s", got.Currency(), currency)
		}

		// A rate at or below 100% can never increase the magnitude.
		if bps <= bpsScale && abs64(got.Amount()) > abs64(amount)+1 {
			t.Fatalf("Money(%d).MulBps(%d) = %d grew in magnitude", amount, bps, got.Amount())
		}
	})
}

// Add and Sub are inverses whenever neither step overflows. This is the property that
// catches a sign or currency-propagation slip.
func TestPropertyAddSubRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		currency := drawCurrency(t)
		a := rapid.Int64Range(math.MinInt64/4, math.MaxInt64/4).Draw(t, "a")
		b := rapid.Int64Range(math.MinInt64/4, math.MaxInt64/4).Draw(t, "b")

		ma, mb := MustNew(a, currency), MustNew(b, currency)

		sum, err := ma.Add(mb)
		if err != nil {
			t.Fatalf("Add(%d, %d) = %v", a, b, err)
		}
		back, err := sum.Sub(mb)
		if err != nil {
			t.Fatalf("Sub = %v", err)
		}
		if !back.Equal(ma) {
			t.Fatalf("(%d + %d) - %d = %d, want %d", a, b, b, back.Amount(), a)
		}
		if sum.Currency() != currency {
			t.Fatalf("currency = %s, want %s", sum.Currency(), currency)
		}
	})
}

// Formatting and parsing round-trip for every amount and every currency, so that the
// exponent table and the two string routines cannot drift apart.
func TestPropertyFormatParseRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		currency := drawCurrency(t)
		amount := rapid.Int64().Draw(t, "amount")

		orig := MustNew(amount, currency)

		s, err := orig.Format()
		if err != nil {
			t.Fatalf("Format(%d %s) = %v", amount, currency, err)
		}
		back, err := ParseDecimal(s, currency)
		if err != nil {
			t.Fatalf("ParseDecimal(%q, %s) = %v", s, currency, err)
		}
		if !back.Equal(orig) {
			t.Fatalf("round trip of %d %s via %q gave %d", amount, currency, s, back.Amount())
		}
	})
}

// Arithmetic either succeeds or reports overflow. It must never wrap around, because a
// wrapped balance is a negative balance that no invariant check would catch.
func TestPropertyArithmeticNeverWraps(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		currency := drawCurrency(t)
		a := rapid.Int64().Draw(t, "a")
		b := rapid.Int64().Draw(t, "b")

		ma, mb := MustNew(a, currency), MustNew(b, currency)

		exactSum := new(big.Int).Add(big.NewInt(a), big.NewInt(b))
		sum, err := ma.Add(mb)
		if fits := exactSum.IsInt64(); fits != (err == nil) {
			t.Fatalf("Add(%d, %d): fits=%v but err=%v", a, b, fits, err)
		} else if fits && sum.Amount() != exactSum.Int64() {
			t.Fatalf("Add(%d, %d) = %d, want %s", a, b, sum.Amount(), exactSum)
		}

		exactDiff := new(big.Int).Sub(big.NewInt(a), big.NewInt(b))
		diff, err := ma.Sub(mb)
		if fits := exactDiff.IsInt64(); fits != (err == nil) {
			t.Fatalf("Sub(%d, %d): fits=%v but err=%v", a, b, fits, err)
		} else if fits && diff.Amount() != exactDiff.Int64() {
			t.Fatalf("Sub(%d, %d) = %d, want %s", a, b, diff.Amount(), exactDiff)
		}
	})
}
