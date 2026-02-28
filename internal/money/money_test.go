package money

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestNewRejectsUnknownCurrency(t *testing.T) {
	if _, err := New(100, "XYZ"); !errors.Is(err, ErrUnknownCurrency) {
		t.Fatalf("New(100, XYZ) error = %v, want ErrUnknownCurrency", err)
	}
	if _, err := New(100, USD); err != nil {
		t.Fatalf("New(100, USD) = %v, want nil", err)
	}
}

// The exponent table is the thing that stops a ¥100 charge becoming ¥10000. The
// zero- and three-decimal cases matter more than the two-decimal one.
func TestExponents(t *testing.T) {
	tests := []struct {
		currency Currency
		want     int32
	}{
		{USD, 2}, {EUR, 2}, {INR, 2},
		{JPY, 0}, {KRW, 0},
		{BHD, 3}, {KWD, 3},
	}
	for _, tc := range tests {
		got, err := tc.currency.Exponent()
		if err != nil {
			t.Errorf("%s.Exponent() = %v, want nil", tc.currency, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s.Exponent() = %d, want %d", tc.currency, got, tc.want)
		}
	}

	if _, err := Currency("XYZ").Exponent(); !errors.Is(err, ErrUnknownCurrency) {
		t.Errorf("XYZ.Exponent() error = %v, want ErrUnknownCurrency", err)
	}
}

func TestAddSub(t *testing.T) {
	a, b := MustNew(9680, USD), MustNew(320, USD)

	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add = %v, want nil", err)
	}
	if sum.Amount() != 10000 {
		t.Errorf("Add amount = %d, want 10000", sum.Amount())
	}

	diff, err := a.Sub(b)
	if err != nil {
		t.Fatalf("Sub = %v, want nil", err)
	}
	if diff.Amount() != 9360 {
		t.Errorf("Sub amount = %d, want 9360", diff.Amount())
	}
}

func TestCurrencyMismatch(t *testing.T) {
	usd, eur := MustNew(100, USD), MustNew(100, EUR)

	if _, err := usd.Add(eur); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Add error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := usd.Sub(eur); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Sub error = %v, want ErrCurrencyMismatch", err)
	}
	if _, err := usd.Cmp(eur); !errors.Is(err, ErrCurrencyMismatch) {
		t.Errorf("Cmp error = %v, want ErrCurrencyMismatch", err)
	}
	// Equal is total: different currencies are simply not equal.
	if usd.Equal(eur) {
		t.Error("Equal across currencies = true, want false")
	}
}

// Overflow must be reported, never wrapped around into a negative balance.
func TestOverflow(t *testing.T) {
	max := MustNew(math.MaxInt64, USD)
	min := MustNew(math.MinInt64, USD)
	one := MustNew(1, USD)

	if _, err := max.Add(one); !errors.Is(err, ErrOverflow) {
		t.Errorf("MaxInt64+1 error = %v, want ErrOverflow", err)
	}
	if _, err := min.Sub(one); !errors.Is(err, ErrOverflow) {
		t.Errorf("MinInt64-1 error = %v, want ErrOverflow", err)
	}
	if _, err := min.Neg(); !errors.Is(err, ErrOverflow) {
		t.Errorf("Neg(MinInt64) error = %v, want ErrOverflow", err)
	}

	// Near-misses must still succeed.
	if _, err := max.Sub(one); err != nil {
		t.Errorf("MaxInt64-1 = %v, want nil", err)
	}
	if _, err := MustNew(math.MinInt64+1, USD).Neg(); err != nil {
		t.Errorf("Neg(MinInt64+1) = %v, want nil", err)
	}
}

func TestSignHelpers(t *testing.T) {
	tests := []struct {
		amount int64
		sign   int
	}{{-500, -1}, {0, 0}, {500, 1}}

	for _, tc := range tests {
		m := MustNew(tc.amount, USD)
		if m.Sign() != tc.sign {
			t.Errorf("Money(%d).Sign() = %d, want %d", tc.amount, m.Sign(), tc.sign)
		}
		if m.IsZero() != (tc.sign == 0) {
			t.Errorf("Money(%d).IsZero() = %v", tc.amount, m.IsZero())
		}
		if m.IsPositive() != (tc.sign > 0) {
			t.Errorf("Money(%d).IsPositive() = %v", tc.amount, m.IsPositive())
		}
		if m.IsNegative() != (tc.sign < 0) {
			t.Errorf("Money(%d).IsNegative() = %v", tc.amount, m.IsNegative())
		}
	}
}

func TestFormat(t *testing.T) {
	tests := []struct {
		minor    int64
		currency Currency
		want     string
	}{
		{9680, USD, "96.80"},
		{5, USD, "0.05"},
		{50, USD, "0.50"},
		{0, USD, "0.00"},
		{-9680, USD, "-96.80"},
		{-5, USD, "-0.05"},
		{100, JPY, "100"},   // zero-decimal: minor unit is the major unit
		{-100, JPY, "-100"}, //
		{1234, BHD, "1.234"},
		{5, BHD, "0.005"},
		{-1234, BHD, "-1.234"},
		{math.MaxInt64, USD, "92233720368547758.07"},
		{math.MinInt64, USD, "-92233720368547758.08"}, // the value with no positive twin
	}

	for _, tc := range tests {
		got, err := MustNew(tc.minor, tc.currency).Format()
		if err != nil {
			t.Errorf("Format(%d %s) = %v, want nil", tc.minor, tc.currency, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Format(%d %s) = %q, want %q", tc.minor, tc.currency, got, tc.want)
		}
	}
}

func TestParseDecimal(t *testing.T) {
	tests := []struct {
		in       string
		currency Currency
		want     int64
	}{
		{"96.80", USD, 9680},
		{"96.8", USD, 9680}, // short fraction is right-padded
		{"96", USD, 9600},
		{"0.05", USD, 5},
		{".05", USD, 5},
		{"-96.80", USD, -9680},
		{"+96.80", USD, 9680},
		{"  96.80  ", USD, 9680},
		{"0", USD, 0},
		{"100", JPY, 100},
		{"1.234", BHD, 1234},
		{"1.2", BHD, 1200},
	}

	for _, tc := range tests {
		got, err := ParseDecimal(tc.in, tc.currency)
		if err != nil {
			t.Errorf("ParseDecimal(%q, %s) = %v, want nil", tc.in, tc.currency, err)
			continue
		}
		if got.Amount() != tc.want {
			t.Errorf("ParseDecimal(%q, %s) = %d, want %d", tc.in, tc.currency, got.Amount(), tc.want)
		}
	}
}

// Too much precision is a caller bug. Rounding it away would hide that bug inside the
// ledger, where it becomes a reconciliation incident instead of a 400.
func TestParseDecimalRejectsExcessPrecision(t *testing.T) {
	tests := []struct {
		in       string
		currency Currency
	}{
		{"1.005", USD},    // three digits for a two-decimal currency
		{"1.0", JPY},      // any fraction at all for a zero-decimal currency
		{"1.2345", BHD},   // four digits for a three-decimal currency
		{"abc", USD},      //
		{"1.2.3", USD},    //
		{"", USD},         //
		{"-", USD},        //
		{"1e5", USD},      // no scientific notation
		{"0x10", USD},     //
		{"1,000.00", USD}, // no thousands separators
	}

	for _, tc := range tests {
		if got, err := ParseDecimal(tc.in, tc.currency); err == nil {
			t.Errorf("ParseDecimal(%q, %s) = %d, want an error", tc.in, tc.currency, got.Amount())
		}
	}
}

func TestParseDecimalRoundTrip(t *testing.T) {
	for _, c := range []Currency{USD, JPY, BHD} {
		// MinInt64 is the value with no positive counterpart: parsing its magnitude
		// unsigned and negating afterwards would reject an amount Format can produce.
		for _, minor := range []int64{
			0, 1, 5, 99, 100, 12345, -1, -99, -12345,
			math.MaxInt64, math.MinInt64, math.MinInt64 + 1,
		} {
			orig := MustNew(minor, c)
			s, err := orig.Format()
			if err != nil {
				t.Fatalf("Format(%d %s) = %v", minor, c, err)
			}
			back, err := ParseDecimal(s, c)
			if err != nil {
				t.Fatalf("ParseDecimal(%q, %s) = %v", s, c, err)
			}
			if !back.Equal(orig) {
				t.Errorf("round trip of %d %s via %q gave %d", minor, c, s, back.Amount())
			}
		}
	}
}

// Money crosses the wire as an integer and a currency. A JSON number that has been
// through a float64 parser has already lost the argument.
func TestJSONRoundTrip(t *testing.T) {
	orig := MustNew(9680, USD)

	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal = %v", err)
	}
	if string(b) != `{"amount":9680,"currency":"USD"}` {
		t.Errorf("Marshal = %s, want {\"amount\":9680,\"currency\":\"USD\"}", b)
	}

	var back Money
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal = %v", err)
	}
	if !back.Equal(orig) {
		t.Errorf("round trip gave %v, want %v", back, orig)
	}
}

func TestJSONRejectsUnknownCurrency(t *testing.T) {
	var m Money
	err := json.Unmarshal([]byte(`{"amount":100,"currency":"XYZ"}`), &m)
	if !errors.Is(err, ErrUnknownCurrency) {
		t.Errorf("Unmarshal error = %v, want ErrUnknownCurrency", err)
	}
}

// A large integer amount must survive JSON without being rounded, which is the failure
// mode of any implementation that goes through float64.
func TestJSONPreservesLargeAmounts(t *testing.T) {
	orig := MustNew(math.MaxInt64, USD)

	b, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("Marshal = %v", err)
	}
	var back Money
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("Unmarshal = %v", err)
	}
	if back.Amount() != math.MaxInt64 {
		t.Errorf("amount = %d, want %d", back.Amount(), int64(math.MaxInt64))
	}
}

func TestParseCurrency(t *testing.T) {
	for _, in := range []string{"usd", "USD", " usd ", "Usd"} {
		got, err := ParseCurrency(in)
		if err != nil {
			t.Errorf("ParseCurrency(%q) = %v, want nil", in, err)
			continue
		}
		if got != USD {
			t.Errorf("ParseCurrency(%q) = %q, want USD", in, got)
		}
	}
	if _, err := ParseCurrency("XYZ"); !errors.Is(err, ErrUnknownCurrency) {
		t.Errorf("ParseCurrency(XYZ) error = %v, want ErrUnknownCurrency", err)
	}
}

// Every currency in the table must have an exponent this package can actually format,
// so that adding a currency without extending pow10 fails here rather than in prod.
func TestEveryCurrencyIsFormattable(t *testing.T) {
	for _, c := range Currencies() {
		exp, err := c.Exponent()
		if err != nil {
			t.Errorf("%s.Exponent() = %v", c, err)
			continue
		}
		if exp < 0 || int(exp) >= len(pow10) {
			t.Errorf("%s has exponent %d, outside the pow10 table", c, exp)
			continue
		}
		if _, err := MustNew(12345, c).Format(); err != nil {
			t.Errorf("Format for %s = %v", c, err)
		}
	}
}

func TestAbs(t *testing.T) {
	tests := []struct {
		in   int64
		want int64
	}{{500, 500}, {-500, 500}, {0, 0}}

	for _, tc := range tests {
		got, err := MustNew(tc.in, USD).Abs()
		if err != nil {
			t.Errorf("Abs(%d) = %v", tc.in, err)
			continue
		}
		if got.Amount() != tc.want {
			t.Errorf("Abs(%d) = %d, want %d", tc.in, got.Amount(), tc.want)
		}
	}

	// MinInt64 has no positive counterpart, so Abs must report rather than wrap.
	if _, err := MustNew(math.MinInt64, USD).Abs(); !errors.Is(err, ErrOverflow) {
		t.Errorf("Abs(MinInt64) = %v, want ErrOverflow", err)
	}
}

func TestString(t *testing.T) {
	if got := MustNew(9680, USD).String(); got != "96.80 USD" {
		t.Errorf("String() = %q, want \"96.80 USD\"", got)
	}
	if got := MustNew(100, JPY).String(); got != "100 JPY" {
		t.Errorf("String() = %q, want \"100 JPY\"", got)
	}

	// An amount in an unknown currency must still render something diagnosable rather
	// than panicking inside a log line.
	if got := (Money{amount: 5, currency: "XYZ"}).String(); !strings.Contains(got, "5") {
		t.Errorf("String() for an unknown currency = %q", got)
	}
}
