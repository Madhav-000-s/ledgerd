package money

import (
	"fmt"
	"sort"
	"strings"
)

// Currency is an ISO-4217 alphabetic code, upper case.
type Currency string

// Common currencies, named so that call sites do not spell string literals.
const (
	USD Currency = "USD"
	EUR Currency = "EUR"
	GBP Currency = "GBP"
	INR Currency = "INR"
	JPY Currency = "JPY" // zero-decimal
	KRW Currency = "KRW" // zero-decimal
	BHD Currency = "BHD" // three-decimal
	KWD Currency = "KWD" // three-decimal
)

// exponents is the compiled-in table of minor-unit exponents. It is deliberately a
// fixed table rather than a runtime lookup: the number of digits in a currency is a
// property of the currency, and a ledger that could disagree with itself about how many
// minor units a dollar has would be unauditable.
//
// The zero- and three-decimal entries are the ones that matter. Code that assumes "two
// decimals" is wrong for a third of the world, and the bugs it produces are off by a
// factor of 100.
var exponents = map[Currency]int32{
	// two-decimal (the common case)
	"AUD": 2, "BRL": 2, "CAD": 2, "CHF": 2, "CNY": 2, "CZK": 2, "DKK": 2,
	"EUR": 2, "GBP": 2, "HKD": 2, "IDR": 2, "ILS": 2, "INR": 2, "MXN": 2,
	"MYR": 2, "NOK": 2, "NZD": 2, "PHP": 2, "PLN": 2, "RON": 2, "RUB": 2,
	"SEK": 2, "SGD": 2, "THB": 2, "TRY": 2, "USD": 2, "ZAR": 2,

	// zero-decimal: the minor unit *is* the major unit. ¥100 is 100, not 10000.
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0, "JPY": 0, "KMF": 0,
	"KRW": 0, "PYG": 0, "RWF": 0, "UGX": 0, "VND": 0, "VUV": 0, "XAF": 0,
	"XOF": 0, "XPF": 0,

	// three-decimal: 1 dinar = 1000 fils.
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
}

// pow10 indexed by exponent, covering every exponent the table above can produce.
var pow10 = [...]int64{1, 10, 100, 1000}

// Exponent returns the number of decimal digits in c's minor unit.
func (c Currency) Exponent() (int32, error) {
	e, ok := exponents[c]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnknownCurrency, string(c))
	}
	return e, nil
}

// Valid reports whether c is a currency this ledger knows how to handle.
func (c Currency) Valid() bool {
	_, ok := exponents[c]
	return ok
}

// String implements fmt.Stringer.
func (c Currency) String() string { return string(c) }

// ParseCurrency validates and normalizes an alphabetic currency code.
func ParseCurrency(s string) (Currency, error) {
	c := Currency(strings.ToUpper(strings.TrimSpace(s)))
	if !c.Valid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownCurrency, s)
	}
	return c, nil
}

// Currencies returns every supported currency, sorted. Used by the API to advertise
// what it accepts, and by property tests to draw from the real table rather than a
// hand-picked subset that would miss the zero- and three-decimal cases.
func Currencies() []Currency {
	out := make([]Currency, 0, len(exponents))
	for c := range exponents {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
