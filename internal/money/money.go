// Package money represents monetary amounts as integer minor units.
//
// The single most important property of this package is that it cannot lose a cent.
// float64 cannot represent 0.1 exactly and silently degrades under accumulation, which
// is exactly the failure a ledger must not have. Decimal string types are correct but
// invite accidental division. int64 in minor units is exact, cheap to compare, and maps
// directly onto a Postgres BIGINT.
//
// There is deliberately no Div. Division is where money leaks: the remainder has to go
// somewhere and a general-purpose Div gives the caller no way to say where. The only
// splitting primitive is Allocate, which distributes a total across ratios using the
// largest-remainder method and guarantees the parts sum back to the total exactly.
package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"sort"
	"strconv"
	"strings"
)

// Sentinel errors. Callers match with errors.Is; the HTTP layer maps them to error
// codes in exactly one table.
var (
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	ErrUnknownCurrency  = errors.New("money: unknown currency")
	ErrOverflow         = errors.New("money: amount overflows int64")
	ErrInvalidAmount    = errors.New("money: invalid amount")
	ErrInvalidRatios    = errors.New("money: invalid allocation ratios")
)

// bpsScale is the denominator for basis points: 1 bp = 1/10000.
const bpsScale = 10000

// halfScale is added before the floor division in MulBps to produce round-half-up.
const halfScale = bpsScale / 2

// Money is an exact monetary amount: a signed count of minor units plus the currency
// those units belong to.
//
// The fields are unexported so that an amount cannot be separated from its currency,
// and so that no caller can construct an amount in a currency this ledger does not
// know the exponent for. The zero value is not a valid amount; use Zero(currency).
type Money struct {
	amount   int64
	currency Currency
}

// New returns an amount of minor units in c.
func New(minor int64, c Currency) (Money, error) {
	if !c.Valid() {
		return Money{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, string(c))
	}
	return Money{amount: minor, currency: c}, nil
}

// MustNew is New for tests and compile-time-known constants. It panics on an unknown
// currency, which in those contexts is a typo rather than a runtime condition.
func MustNew(minor int64, c Currency) Money {
	m, err := New(minor, c)
	if err != nil {
		panic(err)
	}
	return m
}

// Zero returns the zero amount in c.
func Zero(c Currency) Money { return Money{amount: 0, currency: c} }

// Amount returns the signed count of minor units.
func (m Money) Amount() int64 { return m.amount }

// Currency returns the currency.
func (m Money) Currency() Currency { return m.currency }

// IsZero reports whether the amount is zero. It says nothing about the currency.
func (m Money) IsZero() bool { return m.amount == 0 }

// IsPositive reports whether the amount is strictly greater than zero.
func (m Money) IsPositive() bool { return m.amount > 0 }

// IsNegative reports whether the amount is strictly less than zero.
func (m Money) IsNegative() bool { return m.amount < 0 }

// Sign returns -1, 0 or +1.
func (m Money) Sign() int {
	switch {
	case m.amount < 0:
		return -1
	case m.amount > 0:
		return 1
	default:
		return 0
	}
}

// Add returns m+o. Currencies must match; the result must fit in an int64.
func (m Money) Add(o Money) (Money, error) {
	if m.currency != o.currency {
		return Money{}, fmt.Errorf("%w: %s + %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	sum := m.amount + o.amount
	// Overflow iff the operands share a sign and the result does not.
	if (m.amount > 0 && o.amount > 0 && sum < 0) || (m.amount < 0 && o.amount < 0 && sum >= 0) {
		return Money{}, fmt.Errorf("%w: %d + %d", ErrOverflow, m.amount, o.amount)
	}
	return Money{amount: sum, currency: m.currency}, nil
}

// Sub returns m-o. Currencies must match; the result must fit in an int64.
func (m Money) Sub(o Money) (Money, error) {
	if m.currency != o.currency {
		return Money{}, fmt.Errorf("%w: %s - %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	diff := m.amount - o.amount
	if (m.amount >= 0 && o.amount < 0 && diff < 0) || (m.amount < 0 && o.amount > 0 && diff >= 0) {
		return Money{}, fmt.Errorf("%w: %d - %d", ErrOverflow, m.amount, o.amount)
	}
	return Money{amount: diff, currency: m.currency}, nil
}

// Neg returns -m. It errors only on the single unrepresentable value, math.MinInt64.
func (m Money) Neg() (Money, error) {
	if m.amount == math.MinInt64 {
		return Money{}, fmt.Errorf("%w: negating %d", ErrOverflow, m.amount)
	}
	return Money{amount: -m.amount, currency: m.currency}, nil
}

// Abs returns |m|.
func (m Money) Abs() (Money, error) {
	if m.amount >= 0 {
		return m, nil
	}
	return m.Neg()
}

// Cmp compares two amounts of the same currency: -1 if m < o, 0 if equal, +1 if m > o.
func (m Money) Cmp(o Money) (int, error) {
	if m.currency != o.currency {
		return 0, fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.currency, o.currency)
	}
	switch {
	case m.amount < o.amount:
		return -1, nil
	case m.amount > o.amount:
		return 1, nil
	default:
		return 0, nil
	}
}

// Equal reports whether both the amount and the currency match. Unlike Cmp it does not
// error, because "amounts in different currencies are not equal" is a total answer.
func (m Money) Equal(o Money) bool {
	return m.amount == o.amount && m.currency == o.currency
}

// MulBps multiplies by a rate in basis points, rounding half up.
//
//	result = floor((amount*bps + 5000) / 10000)
//
// Rounding half up means ties round toward positive infinity, so the behavior is
// defined for negative amounts rather than accidental. The computation splits the
// amount before multiplying, so it is exact for every int64 amount rather than only
// those small enough that amount*bps happens to fit — that intermediate overflow is the
// classic way a fee calculation silently produces a negative number.
//
// bps must be non-negative, and the result must fit in an int64. Both are properties of
// the fee schedule, which is configuration rather than request data, so a violation is
// a deployment bug and panics rather than returning an error that every call site would
// have to thread. For any rate a fee schedule would actually use — MulBps is exact for
// all |amount| < 2^53 at bps <= 10000 — neither is reachable.
func (m Money) MulBps(bps int64) Money {
	if bps < 0 {
		panic(fmt.Sprintf("money: MulBps with negative rate %d bps", bps))
	}

	// amount = q*10000 + r with 0 <= r < 10000, so
	//   (amount*bps + 5000)/10000 = q*bps + (r*bps + 5000)/10000
	// and the second term is small enough to compute directly.
	q := floorDiv(m.amount, bpsScale)
	r := m.amount - q*bpsScale

	inner, ok := mulInt64(r, bps)
	if !ok {
		panic(fmt.Sprintf("money: MulBps overflow (%d bps of %d)", bps, m.amount))
	}
	inner, ok = addInt64(inner, halfScale)
	if !ok {
		panic(fmt.Sprintf("money: MulBps overflow (%d bps of %d)", bps, m.amount))
	}
	inner = floorDiv(inner, bpsScale)

	outer, ok := mulInt64(q, bps)
	if !ok {
		panic(fmt.Sprintf("money: MulBps overflow (%d bps of %d)", bps, m.amount))
	}
	total, ok := addInt64(outer, inner)
	if !ok {
		panic(fmt.Sprintf("money: MulBps overflow (%d bps of %d)", bps, m.amount))
	}
	return Money{amount: total, currency: m.currency}
}

// Allocate splits m across len(ratios) parts in proportion to ratios, using the
// largest-remainder method. The parts always sum to exactly m — that is the entire
// reason this function exists instead of a Div.
//
//	Allocate(10000, [1,1,1])  -> [3334, 3333, 3333]   sum = 10000
//	Allocate(-10000, [1,1,1]) -> [-3333, -3333, -3334] sum = -10000
//
// The leftover minor units go to the parts with the largest fractional remainder. Ties
// are broken by position, sweeping forward for a positive total and backward for a
// negative one, so that Allocate(-n, r) is the mirror image of Allocate(n, r) rather
// than an unrelated distribution.
//
// Ratios must be non-negative and must not all be zero. Like MulBps, a split is
// configuration rather than request data, so a malformed one is a deployment bug and
// panics. ValidateRatios exists for call sites that build a split from input and want
// to reject it at the boundary instead.
func (m Money) Allocate(ratios []int64) []Money {
	sum, err := ValidateRatios(ratios)
	if err != nil {
		panic("money: " + err.Error())
	}

	parts := make([]int64, len(ratios))
	rems := make([]int64, len(ratios)) // |remainder| of each truncated division
	var allocated int64

	for i, ratio := range ratios {
		q, rem := mulDivTrunc(m.amount, ratio, sum)
		parts[i] = q
		rems[i] = abs64(rem)
		// Truncation keeps |q| <= |m.amount|, so this sum cannot overflow.
		allocated += q
	}

	// leftover carries the sign of the total and |leftover| < len(ratios).
	leftover := m.amount - allocated
	if leftover != 0 {
		step := int64(1)
		if leftover < 0 {
			step = -1
		}

		order := make([]int, len(ratios))
		for i := range order {
			order[i] = i
		}
		forward := leftover > 0
		sort.SliceStable(order, func(a, b int) bool {
			ia, ib := order[a], order[b]
			if rems[ia] != rems[ib] {
				return rems[ia] > rems[ib] // largest remainder first
			}
			if forward {
				return ia < ib
			}
			return ia > ib
		})

		// Truncation loses strictly less than one unit per part, so |leftover| is
		// always smaller than len(order) and this cannot run off the end.
		for i := int64(0); i < abs64(leftover); i++ {
			parts[order[i]] += step
		}
	}

	out := make([]Money, len(parts))
	for i, p := range parts {
		out[i] = Money{amount: p, currency: m.currency}
	}
	return out
}

// ValidateRatios checks an allocation split and returns the sum of the ratios. Call
// sites that build a split from untrusted input should use this to reject it at the
// boundary, rather than letting Allocate panic deeper in.
func ValidateRatios(ratios []int64) (int64, error) {
	if len(ratios) == 0 {
		return 0, fmt.Errorf("%w: no ratios given", ErrInvalidRatios)
	}

	var sum int64
	for i, r := range ratios {
		if r < 0 {
			return 0, fmt.Errorf("%w: ratios[%d] = %d is negative", ErrInvalidRatios, i, r)
		}
		var ok bool
		if sum, ok = addInt64(sum, r); !ok {
			return 0, fmt.Errorf("%w: ratios sum overflows int64", ErrInvalidRatios)
		}
	}
	if sum == 0 {
		return 0, fmt.Errorf("%w: ratios sum to zero", ErrInvalidRatios)
	}
	return sum, nil
}

// String renders the amount with its currency, e.g. "96.80 USD" or "100 JPY".
func (m Money) String() string {
	s, err := m.Format()
	if err != nil {
		return fmt.Sprintf("%d minor %s", m.amount, m.currency)
	}
	return s + " " + string(m.currency)
}

// Format renders just the decimal amount, with the number of digits the currency
// actually has: two for USD, none for JPY, three for BHD.
func (m Money) Format() (string, error) {
	exp, err := m.currency.Exponent()
	if err != nil {
		return "", err
	}
	if exp == 0 {
		return strconv.FormatInt(m.amount, 10), nil
	}

	scale := pow10[exp]
	neg := m.amount < 0

	// Take the magnitude in a way that survives math.MinInt64.
	mag := uint64(m.amount)
	if neg {
		mag = -mag
	}
	whole := mag / uint64(scale)
	frac := mag % uint64(scale)

	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString(strconv.FormatUint(whole, 10))
	b.WriteByte('.')
	fs := strconv.FormatUint(frac, 10)
	for i := len(fs); i < int(exp); i++ {
		b.WriteByte('0')
	}
	b.WriteString(fs)
	return b.String(), nil
}

// ParseDecimal parses a decimal string such as "96.80" into minor units of c. It
// rejects anything with more fractional digits than the currency has, rather than
// rounding: a caller that sends "1.005" to a two-decimal currency has a bug, and
// silently rounding it would hide the bug inside the ledger.
func ParseDecimal(s string, c Currency) (Money, error) {
	exp, err := c.Exponent()
	if err != nil {
		return Money{}, err
	}

	s = strings.TrimSpace(s)
	if s == "" {
		return Money{}, fmt.Errorf("%w: empty string", ErrInvalidAmount)
	}

	neg := false
	switch s[0] {
	case '-':
		neg, s = true, s[1:]
	case '+':
		s = s[1:]
	}
	if s == "" {
		return Money{}, fmt.Errorf("%w: sign with no digits", ErrInvalidAmount)
	}

	whole, frac, hasDot := strings.Cut(s, ".")
	if hasDot && exp == 0 {
		return Money{}, fmt.Errorf("%w: %s has no minor units, got %q", ErrInvalidAmount, c, s)
	}
	if len(frac) > int(exp) {
		return Money{}, fmt.Errorf("%w: %s has %d decimal places, got %q", ErrInvalidAmount, c, exp, s)
	}
	if whole == "" && frac == "" {
		return Money{}, fmt.Errorf("%w: no digits in %q", ErrInvalidAmount, s)
	}
	if !allDigits(whole) || !allDigits(frac) {
		return Money{}, fmt.Errorf("%w: %q is not a decimal number", ErrInvalidAmount, s)
	}

	// Right-pad the fraction so "96.8" and "96.80" agree.
	padded := frac + strings.Repeat("0", int(exp)-len(frac))
	digits := whole + padded
	if digits == "" {
		digits = "0"
	}
	// Parse with the sign attached rather than negating afterwards: the magnitude of
	// MinInt64 has no positive counterpart, so parsing it unsigned would reject the one
	// value Format is able to produce but ParseDecimal could not read back.
	if neg {
		digits = "-" + digits
	}

	minor, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return Money{}, fmt.Errorf("%w: %q does not fit in int64", ErrOverflow, s)
	}
	return Money{amount: minor, currency: c}, nil
}

// jsonMoney is the wire representation: an integer plus a currency, never a decimal
// string and never a float. A JSON number that has passed through a float64 parser has
// already lost the argument.
type jsonMoney struct {
	Amount   int64    `json:"amount"`
	Currency Currency `json:"currency"`
}

// MarshalJSON implements json.Marshaler.
func (m Money) MarshalJSON() ([]byte, error) {
	return json.Marshal(jsonMoney{Amount: m.amount, Currency: m.currency})
}

// UnmarshalJSON implements json.Unmarshaler, validating the currency on the way in.
func (m *Money) UnmarshalJSON(b []byte) error {
	var j jsonMoney
	if err := json.Unmarshal(b, &j); err != nil {
		return err
	}
	if !j.Currency.Valid() {
		return fmt.Errorf("%w: %q", ErrUnknownCurrency, string(j.Currency))
	}
	m.amount, m.currency = j.Amount, j.Currency
	return nil
}

// ── integer helpers ─────────────────────────────────────────────────────────

// floorDiv divides rounding toward negative infinity. Go's / truncates toward zero,
// which would make MulBps round differently either side of zero.
func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		q--
	}
	return q
}

// mulInt64 multiplies with overflow detection.
func mulInt64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	p := a * b
	// Division recovers the operand iff no wraparound occurred. The MinInt64/-1 case is
	// excluded explicitly because that division itself traps.
	if (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
		return 0, false
	}
	if p/b != a {
		return 0, false
	}
	return p, true
}

// addInt64 adds with overflow detection.
func addInt64(a, b int64) (int64, bool) {
	s := a + b
	if (a > 0 && b > 0 && s < 0) || (a < 0 && b < 0 && s >= 0) {
		return 0, false
	}
	return s, true
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// mulDivTrunc computes a*ratio/div and the remainder, truncating toward zero, using a
// 128-bit intermediate so that a*ratio cannot overflow. It requires ratio >= 0,
// div > 0 and ratio <= div, which together bound the quotient by |a| and so guarantee
// the result fits in an int64. Allocate satisfies all three by construction.
func mulDivTrunc(a, ratio, div int64) (q, rem int64) {
	if a == 0 || ratio == 0 {
		return 0, 0
	}

	neg := a < 0
	// uint64(a) is the two's-complement magnitude for MinInt64 after negation, so this
	// handles the one value that has no positive counterpart.
	ua := uint64(a)
	if neg {
		ua = -ua
	}

	hi, lo := bits.Mul64(ua, uint64(ratio))
	if hi >= uint64(div) {
		// Unreachable while ratio <= div; guarding keeps a future caller from getting a
		// panic out of bits.Div64 instead of an obviously wrong number.
		panic(fmt.Sprintf("money: allocate quotient overflows (a=%d ratio=%d div=%d)", a, ratio, div))
	}
	uq, ur := bits.Div64(hi, lo, uint64(div))

	q, rem = int64(uq), int64(ur)
	if neg {
		q, rem = -q, -rem
	}
	return q, rem
}

func allDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
