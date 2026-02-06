package money

import (
	"math/big"
	"testing"
)

func amounts(ms []Money) []int64 {
	out := make([]int64, len(ms))
	for i, m := range ms {
		out[i] = m.Amount()
	}
	return out
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sumInt64s(xs []int64) int64 {
	var s int64
	for _, x := range xs {
		s += x
	}
	return s
}

// The two cases stated in the design doc. The remainder has to land somewhere, and
// where it lands is a decision, not an accident.
func TestAllocateDesignCases(t *testing.T) {
	tests := []struct {
		name   string
		total  int64
		ratios []int64
		want   []int64
	}{
		{"thirds of 100.00", 10000, []int64{1, 1, 1}, []int64{3334, 3333, 3333}},
		{"thirds of -100.00", -10000, []int64{1, 1, 1}, []int64{-3333, -3333, -3334}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := amounts(MustNew(tc.total, USD).Allocate(tc.ratios))
			if !equalInt64s(got, tc.want) {
				t.Errorf("Allocate(%d, %v) = %v, want %v", tc.total, tc.ratios, got, tc.want)
			}
			if s := sumInt64s(got); s != tc.total {
				t.Errorf("parts sum to %d, want %d", s, tc.total)
			}
		})
	}
}

// When every ratio is equal, the parts are interchangeable and the only thing that
// distinguishes them is where the leftover landed. Sweeping backward for a negative
// total makes that case the exact mirror of the positive one, which is the property the
// design doc's two examples state.
//
// This does not generalize to unequal ratios: part i belongs to ratio i, so there is
// nothing to reverse. The guarantee for those is exactness of the sum plus fairness,
// covered by TestAllocateSumsExactly and TestAllocateIsWithinOneUnitOfExactShare.
func TestAllocateEqualRatiosMirror(t *testing.T) {
	ratioSets := [][]int64{
		{1, 1, 1},
		{50, 50},
		{1, 1, 1, 1},
		{3, 3, 3, 3, 3},
	}

	for _, ratios := range ratioSets {
		for _, total := range []int64{1, 7, 100, 9999, 10000, 123457} {
			pos := amounts(MustNew(total, USD).Allocate(ratios))
			neg := amounts(MustNew(-total, USD).Allocate(ratios))

			want := make([]int64, len(pos))
			for i, p := range pos {
				want[len(pos)-1-i] = -p
			}
			if !equalInt64s(neg, want) {
				t.Errorf("Allocate(%d, %v) = %v; Allocate(%d, %v) = %v, want %v",
					total, ratios, pos, -total, ratios, neg, want)
			}
		}
	}
}

// Fairness: no part may differ from its exact proportional share by a whole minor unit.
// Summing to the total is necessary but not sufficient — an implementation that handed
// the entire amount to the first part and zero to the rest would also sum correctly.
//
// Checked as |part*sum - total*ratio| < sum in exact arithmetic, to avoid restating the
// production rounding logic in the test.
func TestAllocateIsWithinOneUnitOfExactShare(t *testing.T) {
	ratioSets := [][]int64{
		{1, 1, 1},
		{1, 2, 3},
		{50, 50},
		{7, 11, 13, 17},
		{1, 0, 1},
		{97, 1, 1, 1},
	}
	totals := []int64{1, -1, 7, -7, 100, -100, 9999, -9999, 123457, -123457}

	for _, ratios := range ratioSets {
		var sum int64
		for _, r := range ratios {
			sum += r
		}
		bigSum := big.NewInt(sum)

		for _, total := range totals {
			parts := amounts(MustNew(total, USD).Allocate(ratios))

			for i, part := range parts {
				// lhs = part*sum, rhs = total*ratio
				lhs := new(big.Int).Mul(big.NewInt(part), bigSum)
				rhs := new(big.Int).Mul(big.NewInt(total), big.NewInt(ratios[i]))
				diff := new(big.Int).Abs(new(big.Int).Sub(lhs, rhs))

				if diff.Cmp(bigSum) >= 0 {
					t.Errorf("Allocate(%d, %v) = %v: part %d = %d is more than one minor unit from its exact share %d/%d",
						total, ratios, parts, i, part, total*ratios[i], sum)
				}
			}
		}
	}
}

// A larger ratio must never receive a smaller part. This is the ordering property that
// a sloppy leftover distribution breaks.
func TestAllocateRespectsRatioOrder(t *testing.T) {
	ratios := []int64{1, 2, 3, 10}

	for _, total := range []int64{0, 1, 7, 100, 9999, 123457} {
		parts := amounts(MustNew(total, USD).Allocate(ratios))
		for i := 1; i < len(parts); i++ {
			if parts[i] < parts[i-1] {
				t.Errorf("Allocate(%d, %v) = %v: ratio %d got less than ratio %d",
					total, ratios, parts, ratios[i], ratios[i-1])
			}
		}
	}
}

func TestAllocateSumsExactly(t *testing.T) {
	ratioSets := [][]int64{
		{1},
		{1, 1},
		{1, 1, 1},
		{1, 2, 3, 4},
		{97, 1, 1, 1},
		{0, 0, 1},
		{1000000, 1},
	}
	totals := []int64{0, 1, -1, 2, 99, 100, 101, -99, 10000, -10000, 999999937}

	for _, ratios := range ratioSets {
		for _, total := range totals {
			got := amounts(MustNew(total, USD).Allocate(ratios))
			if len(got) != len(ratios) {
				t.Fatalf("Allocate(%d, %v) returned %d parts, want %d", total, ratios, len(got), len(ratios))
			}
			if s := sumInt64s(got); s != total {
				t.Errorf("Allocate(%d, %v) = %v sums to %d, want %d", total, ratios, got, s, total)
			}
		}
	}
}

// A zero ratio must receive nothing, not a rounding crumb.
func TestAllocateZeroRatioGetsNothing(t *testing.T) {
	got := amounts(MustNew(10000, USD).Allocate([]int64{1, 0, 1}))
	if got[1] != 0 {
		t.Errorf("Allocate(10000, [1 0 1]) = %v, want the zero-ratio part to be 0", got)
	}
	if s := sumInt64s(got); s != 10000 {
		t.Errorf("parts sum to %d, want 10000", s)
	}
}

func TestAllocatePreservesCurrency(t *testing.T) {
	for _, c := range []Currency{USD, JPY, BHD} {
		for _, part := range MustNew(1000, c).Allocate([]int64{1, 1, 1}) {
			if part.Currency() != c {
				t.Errorf("part currency = %s, want %s", part.Currency(), c)
			}
		}
	}
}

// Allocate is exact even when total*ratio would overflow an int64 on the way through,
// because it divides in a 128-bit intermediate.
func TestAllocateLargeAmountsDoNotOverflow(t *testing.T) {
	const huge = int64(9_000_000_000_000_000_000)

	got := amounts(MustNew(huge, USD).Allocate([]int64{1_000_000, 1_000_000, 1_000_000}))
	if s := sumInt64s(got); s != huge {
		t.Errorf("parts sum to %d, want %d", s, huge)
	}
	for i, p := range got {
		if p < 0 {
			t.Errorf("part %d = %d is negative; an intermediate overflowed", i, p)
		}
	}
}

func TestAllocateRejectsBadRatios(t *testing.T) {
	tests := []struct {
		name   string
		ratios []int64
	}{
		{"empty", []int64{}},
		{"nil", nil},
		{"all zero", []int64{0, 0}},
		{"negative", []int64{1, -1}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ValidateRatios(tc.ratios); err == nil {
				t.Errorf("ValidateRatios(%v) = nil, want an error", tc.ratios)
			}
			defer func() {
				if recover() == nil {
					t.Errorf("Allocate(%v) did not panic", tc.ratios)
				}
			}()
			MustNew(100, USD).Allocate(tc.ratios)
		})
	}
}

// The fee table from the design doc: 2.9% + 30c. The last row is the one that matters —
// a fee that exceeds the charge is a validation error at the payments boundary, not a
// negative transfer, and money must not quietly hide it.
func TestMulBpsFeeSchedule(t *testing.T) {
	const (
		bps   = int64(290)
		fixed = int64(30)
	)

	tests := []struct {
		amount int64
		fee    int64
		net    int64
	}{
		{10000, 320, 9680},
		{999, 59, 940},
		{12345, 388, 11957},
		{50, 31, 19},
		{1, 30, -29}, // fee exceeds the charge
	}

	for _, tc := range tests {
		amount := MustNew(tc.amount, USD)

		fee, err := amount.MulBps(bps).Add(MustNew(fixed, USD))
		if err != nil {
			t.Fatalf("fee for %d = %v", tc.amount, err)
		}
		if fee.Amount() != tc.fee {
			t.Errorf("fee for %d = %d, want %d", tc.amount, fee.Amount(), tc.fee)
		}

		net, err := amount.Sub(fee)
		if err != nil {
			t.Fatalf("net for %d = %v", tc.amount, err)
		}
		if net.Amount() != tc.net {
			t.Errorf("net for %d = %d, want %d", tc.amount, net.Amount(), tc.net)
		}
	}
}

func TestMulBpsRoundsHalfUp(t *testing.T) {
	tests := []struct {
		amount int64
		bps    int64
		want   int64
	}{
		{100, 10000, 100}, // 100%
		{100, 5000, 50},   // 50%
		{100, 0, 0},       // 0%
		{1, 5000, 1},      // exactly 0.5 rounds up
		{3, 5000, 2},      // exactly 1.5 rounds up
		{1, 4999, 0},      // just under a half rounds down
		{1, 5001, 1},      // just over a half rounds up
		// Ties round toward positive infinity on both sides of zero, which is what
		// "half up" means; -0.5 therefore becomes 0, not -1.
		{-1, 5000, 0},
		{-3, 5000, -1},
		{-100, 5000, -50},
		{-101, 5000, -50}, // -50.5 -> -50
	}

	for _, tc := range tests {
		got := MustNew(tc.amount, USD).MulBps(tc.bps)
		if got.Amount() != tc.want {
			t.Errorf("Money(%d).MulBps(%d) = %d, want %d", tc.amount, tc.bps, got.Amount(), tc.want)
		}
	}
}

// The intermediate amount*bps overflows an int64 well before the result does. Splitting
// the amount before multiplying is what keeps a large charge from producing a negative
// fee.
func TestMulBpsLargeAmountsDoNotOverflow(t *testing.T) {
	tests := []struct {
		amount int64
		bps    int64
		want   int64
	}{
		{1 << 53, 10000, 1 << 53}, // amount*bps would be ~9.0e19
		{9_000_000_000_000_000_000, 10000, 9_000_000_000_000_000_000},
		{9_000_000_000_000_000_000, 290, 261_000_000_000_000_000},
		{-9_000_000_000_000_000_000, 290, -261_000_000_000_000_000},
	}

	for _, tc := range tests {
		got := MustNew(tc.amount, USD).MulBps(tc.bps)
		if got.Amount() != tc.want {
			t.Errorf("Money(%d).MulBps(%d) = %d, want %d", tc.amount, tc.bps, got.Amount(), tc.want)
		}
	}
}

func TestMulBpsPreservesCurrency(t *testing.T) {
	got := MustNew(10000, JPY).MulBps(290)
	if got.Currency() != JPY {
		t.Errorf("currency = %s, want JPY", got.Currency())
	}
}

func TestMulBpsRejectsNegativeRate(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("MulBps(-1) did not panic")
		}
	}()
	MustNew(100, USD).MulBps(-1)
}
