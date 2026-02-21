package payments

import (
	"errors"
	"fmt"

	"github.com/Madhav-000-s/ledgerd/internal/money"
)

// Sentinel errors.
var (
	// ErrFeeExceedsAmount means the schedule produced a fee at or above the charge.
	//
	// This is the edge case from DESIGN.md D1: at 2.9% + 30c, a 1c charge yields a 30c
	// fee. The correct answer is to refuse the charge at the boundary, not to post a
	// transaction whose net is negative — a negative payable is a merchant who now owes
	// the platform money because they accepted a penny.
	ErrFeeExceedsAmount = errors.New("payments: fee is greater than or equal to the amount")

	// ErrInvalidFeeSchedule means the configured rate is nonsensical.
	ErrInvalidFeeSchedule = errors.New("payments: invalid fee schedule")

	// ErrPaymentNotFound means no payment exists with that id.
	ErrPaymentNotFound = errors.New("payments: payment not found")

	// ErrNotCapturable means the payment is not an outstanding authorization.
	ErrNotCapturable = errors.New("payments: payment is not awaiting capture")

	// ErrNotRefundable means the payment has not been captured, or was already fully
	// refunded.
	ErrNotRefundable = errors.New("payments: payment cannot be refunded")

	// ErrRefundExceedsPayment means the cumulative refunds would exceed the charge.
	ErrRefundExceedsPayment = errors.New("payments: refund exceeds the remaining amount")

	// ErrAmountRequired means a required amount was missing or non-positive.
	ErrAmountRequired = errors.New("payments: a positive amount is required")
)

// FeeSchedule is the platform's cut: a rate in basis points plus a fixed component.
type FeeSchedule struct {
	// Bps is the percentage component in basis points. 290 is 2.9%.
	Bps int64
	// Fixed is the flat component in minor units.
	Fixed int64
}

// Validate checks the schedule is usable.
func (f FeeSchedule) Validate() error {
	if f.Bps < 0 || f.Bps > 10_000 {
		return fmt.Errorf("%w: bps must be between 0 and 10000, got %d", ErrInvalidFeeSchedule, f.Bps)
	}
	if f.Fixed < 0 {
		return fmt.Errorf("%w: fixed must not be negative, got %d", ErrInvalidFeeSchedule, f.Fixed)
	}
	return nil
}

// Compute returns the fee for an amount and the net owed to the merchant.
//
// The percentage component rounds half up on an exact integer path; the fixed component
// is added afterwards. A fee at or above the amount is rejected here, at the domain
// boundary, rather than becoming a negative transfer inside the ledger.
func (f FeeSchedule) Compute(amount money.Money) (fee, net money.Money, err error) {
	if err := f.Validate(); err != nil {
		return money.Money{}, money.Money{}, err
	}
	if amount.Amount() <= 0 {
		return money.Money{}, money.Money{}, fmt.Errorf("%w: got %d", ErrAmountRequired, amount.Amount())
	}

	fixed, err := money.New(f.Fixed, amount.Currency())
	if err != nil {
		return money.Money{}, money.Money{}, err
	}

	fee, err = amount.MulBps(f.Bps).Add(fixed)
	if err != nil {
		return money.Money{}, money.Money{}, err
	}

	cmp, err := fee.Cmp(amount)
	if err != nil {
		return money.Money{}, money.Money{}, err
	}
	if cmp >= 0 {
		return money.Money{}, money.Money{}, fmt.Errorf("%w: fee %d on an amount of %d",
			ErrFeeExceedsAmount, fee.Amount(), amount.Amount())
	}

	net, err = amount.Sub(fee)
	if err != nil {
		return money.Money{}, money.Money{}, err
	}
	return fee, net, nil
}

// RefundFee returns how much of a fee comes back on this refund.
//
// The default policy retains the fixed component and returns the percentage component
// pro rata: the platform did the work of processing the charge either way, but it should
// not keep a percentage of money the merchant no longer has.
//
// alreadyRefunded is required because the answer cannot be computed from this refund
// alone. Rounding each partial refund independently accumulates error — refunding
// 10000 as 3333 + 3333 + 3334 returns 291 of a 290 fee that way, and the extra cent is
// created out of nothing. Instead the fee owed for everything refunded so far is
// computed, and what has already been returned is subtracted. The differences telescope,
// so a payment refunded in any number of pieces returns exactly the percentage fee and
// never a cent more or less.
//
// refundPercentage=false retains the whole fee, which is the other defensible policy and
// is a configuration flag rather than a code change.
func (f FeeSchedule) RefundFee(charged, alreadyRefunded, refundAmount money.Money, refundPercentage bool) (money.Money, error) {
	zero := money.Zero(charged.Currency())
	if !refundPercentage {
		return zero, nil
	}
	if charged.Amount() <= 0 {
		return zero, fmt.Errorf("%w: charged amount is %d", ErrAmountRequired, charged.Amount())
	}

	total := alreadyRefunded.Amount() + refundAmount.Amount()
	if total > charged.Amount() {
		return zero, fmt.Errorf("%w: refunding %d of %d in total", ErrRefundExceedsPayment,
			total, charged.Amount())
	}

	percentageFee := charged.MulBps(f.Bps)
	if percentageFee.IsZero() {
		return zero, nil
	}

	owedNow := feeForRefunded(percentageFee, charged.Amount(), total)
	owedBefore := feeForRefunded(percentageFee, charged.Amount(), alreadyRefunded.Amount())

	return money.New(owedNow-owedBefore, charged.Currency())
}

// feeForRefunded is how much of the percentage fee is attributable to having refunded
// `refunded` of `charged`. Allocate is used rather than a division so the split is exact
// and the two sides always sum back to the whole fee.
func feeForRefunded(percentageFee money.Money, charged, refunded int64) int64 {
	switch {
	case refunded <= 0:
		return 0
	case refunded >= charged:
		return percentageFee.Amount()
	default:
		parts := percentageFee.Allocate([]int64{refunded, charged - refunded})
		return parts[0].Amount()
	}
}
