package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/ledger/memory"
	"github.com/Madhav-000-s/ledgerd/internal/money"
)

const merchant = "mer_test"

// The design doc's schedule: 2.9% + 30c.
var stripeish = FeeSchedule{Bps: 290, Fixed: 30}

type fixture struct {
	t     *testing.T
	ctx   context.Context
	store *memory.Store
	svc   *Service
	l     ledger.Service
}

func newFixture(t *testing.T, fees FeeSchedule, refundPercentage bool) *fixture {
	t.Helper()

	store := memory.New()
	l := ledger.NewService(store)

	svc, err := NewService(l, Options{Fees: fees, RefundPercentageFee: refundPercentage})
	if err != nil {
		t.Fatalf("NewService = %v", err)
	}
	return &fixture{t: t, ctx: context.Background(), store: store, svc: svc, l: l}
}

func usd(n int64) money.Money { return money.MustNew(n, money.USD) }

func (f *fixture) charge(amount int64, capture bool) *Payment {
	f.t.Helper()

	p, err := f.svc.Charge(f.ctx, ChargeRequest{
		MerchantID: merchant,
		Amount:     usd(amount),
		Capture:    capture,
	})
	if err != nil {
		f.t.Fatalf("Charge(%d) = %v", amount, err)
	}
	return p
}

func (f *fixture) chart() *Chart {
	f.t.Helper()

	c, err := EnsureChart(f.ctx, f.l, merchant, money.USD)
	if err != nil {
		f.t.Fatalf("EnsureChart = %v", err)
	}
	return c
}

func (f *fixture) balance(accountID string) int64 {
	f.t.Helper()

	b, err := f.l.Balance(f.ctx, accountID)
	if err != nil {
		f.t.Fatalf("Balance = %v", err)
	}
	return b.Posted
}

// ── fees ────────────────────────────────────────────────────────────────────

// The verified table from DESIGN.md D1. The last row is the one that matters: a fee at
// or above the charge must be refused, not posted as a negative net.
func TestFeeSchedule(t *testing.T) {
	tests := []struct {
		amount  int64
		fee     int64
		net     int64
		wantErr error
	}{
		{10000, 320, 9680, nil},
		{999, 59, 940, nil},
		{12345, 388, 11957, nil},
		{50, 31, 19, nil},
		{1, 0, 0, ErrFeeExceedsAmount}, // fee would be 30 on a 1c charge
	}

	for _, tc := range tests {
		fee, net, err := stripeish.Compute(usd(tc.amount))

		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("Compute(%d) error = %v, want %v", tc.amount, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("Compute(%d) = %v", tc.amount, err)
			continue
		}
		if fee.Amount() != tc.fee {
			t.Errorf("Compute(%d) fee = %d, want %d", tc.amount, fee.Amount(), tc.fee)
		}
		if net.Amount() != tc.net {
			t.Errorf("Compute(%d) net = %d, want %d", tc.amount, net.Amount(), tc.net)
		}
		if fee.Amount()+net.Amount() != tc.amount {
			t.Errorf("Compute(%d): fee + net = %d, does not reconstruct the amount",
				tc.amount, fee.Amount()+net.Amount())
		}
	}
}

func TestFeeScheduleValidation(t *testing.T) {
	for _, f := range []FeeSchedule{{Bps: -1}, {Bps: 10_001}, {Fixed: -5}} {
		if err := f.Validate(); !errors.Is(err, ErrInvalidFeeSchedule) {
			t.Errorf("Validate(%+v) = %v, want ErrInvalidFeeSchedule", f, err)
		}
	}
	if err := (FeeSchedule{Bps: 290, Fixed: 30}).Validate(); err != nil {
		t.Errorf("a valid schedule failed validation: %v", err)
	}
}

// Refunding a payment in pieces must return exactly the percentage fee across all of
// them, never a cent more.
func TestRefundFeeSumsExactly(t *testing.T) {
	charged := usd(10000)
	percentage := charged.MulBps(stripeish.Bps) // 290

	var (
		returned int64
		already  int64
	)
	for _, part := range []int64{3333, 3333, 3334} {
		fee, err := stripeish.RefundFee(charged, usd(already), usd(part), true)
		if err != nil {
			t.Fatalf("RefundFee = %v", err)
		}
		returned += fee.Amount()
		already += part
	}

	if returned != percentage.Amount() {
		t.Errorf("three partial refunds returned %d of the fee, want %d", returned, percentage.Amount())
	}
}

func TestRefundFeeRetentionPolicy(t *testing.T) {
	charged := usd(10000)

	returned, err := stripeish.RefundFee(charged, usd(0), charged, true)
	if err != nil {
		t.Fatalf("RefundFee = %v", err)
	}
	// The percentage component comes back; the fixed 30c does not.
	if returned.Amount() != 290 {
		t.Errorf("returned fee = %d, want 290 (percentage only)", returned.Amount())
	}

	retained, err := stripeish.RefundFee(charged, usd(0), charged, false)
	if err != nil {
		t.Fatalf("RefundFee = %v", err)
	}
	if !retained.IsZero() {
		t.Errorf("with the retention policy the returned fee = %d, want 0", retained.Amount())
	}
}

// ── charges ─────────────────────────────────────────────────────────────────

// The movement from DESIGN.md §5.1.
func TestChargePostsTheDocumentedSplit(t *testing.T) {
	f := newFixture(t, stripeish, true)

	p := f.charge(10000, true)
	chart := f.chart()

	if p.State != StateSucceeded {
		t.Errorf("state = %q, want succeeded", p.State)
	}
	if p.Amount.Amount() != 10000 {
		t.Errorf("amount = %d, want 10000", p.Amount.Amount())
	}
	if p.Fee.Amount() != 320 {
		t.Errorf("fee = %d, want 320", p.Fee.Amount())
	}
	if p.Net.Amount() != 9680 {
		t.Errorf("net = %d, want 9680", p.Net.Amount())
	}

	if got := f.balance(chart.Clearing); got != 10000 {
		t.Errorf("clearing = %d, want 10000", got)
	}
	if got := f.balance(chart.Payable); got != 9680 {
		t.Errorf("payable = %d, want 9680", got)
	}
	if got := f.balance(chart.FeeRevenue); got != 320 {
		t.Errorf("fee_revenue = %d, want 320", got)
	}
}

// The whole point of D1's last row: a 1c charge under a 2.9% + 30c schedule must be
// refused rather than leaving the merchant owing 29c for having accepted a penny.
func TestChargeRefusesWhenTheFeeExceedsTheAmount(t *testing.T) {
	f := newFixture(t, stripeish, true)

	_, err := f.svc.Charge(f.ctx, ChargeRequest{MerchantID: merchant, Amount: usd(1)})
	if !errors.Is(err, ErrFeeExceedsAmount) {
		t.Fatalf("Charge(1) = %v, want ErrFeeExceedsAmount", err)
	}

	// Nothing was written.
	if n := len(f.store.AllEntries()); n != 0 {
		t.Errorf("%d entries written for a refused charge, want 0", n)
	}
}

func TestChargeRejectsNonPositiveAmounts(t *testing.T) {
	f := newFixture(t, stripeish, true)

	for _, amount := range []int64{0, -100} {
		if _, err := f.svc.Charge(f.ctx, ChargeRequest{MerchantID: merchant, Amount: usd(amount)}); err == nil {
			t.Errorf("Charge(%d) = nil, want an error", amount)
		}
	}
}

// ── authorization, capture and cancel ───────────────────────────────────────

func TestAuthorizeThenCaptureInFull(t *testing.T) {
	f := newFixture(t, stripeish, true)
	chart := f.chart()

	auth := f.charge(10000, false)
	if auth.State != StateRequiresCapture {
		t.Fatalf("state = %q, want requires_capture", auth.State)
	}
	// A hold moves nothing posted.
	if got := f.balance(chart.Clearing); got != 0 {
		t.Errorf("clearing = %d after an authorization, want 0", got)
	}

	captured, err := f.svc.Capture(f.ctx, merchant, auth.ID, nil)
	if err != nil {
		t.Fatalf("Capture = %v", err)
	}

	if captured.State != StateSucceeded {
		t.Errorf("state = %q, want succeeded", captured.State)
	}
	if got := f.balance(chart.Clearing); got != 10000 {
		t.Errorf("clearing = %d, want 10000", got)
	}
	if got := f.balance(chart.Payable); got != 9680 {
		t.Errorf("payable = %d, want 9680", got)
	}
	// The hold is fully released.
	if got := f.balance(chart.Holds); got != 0 {
		t.Errorf("holds = %d, want 0", got)
	}
}

// A partial capture charges a fee on what was actually taken, not on what was
// authorized.
func TestPartialCapture(t *testing.T) {
	f := newFixture(t, stripeish, true)
	chart := f.chart()

	auth := f.charge(10000, false)

	amount := usd(6000)
	captured, err := f.svc.Capture(f.ctx, merchant, auth.ID, &amount)
	if err != nil {
		t.Fatalf("Capture = %v", err)
	}

	if captured.Amount.Amount() != 6000 {
		t.Errorf("amount = %d, want 6000", captured.Amount.Amount())
	}
	// 2.9% of 6000 is 174, plus 30 = 204.
	if captured.Fee.Amount() != 204 {
		t.Errorf("fee = %d, want 204 (charged on what was captured)", captured.Fee.Amount())
	}
	if captured.Net.Amount() != 5796 {
		t.Errorf("net = %d, want 5796", captured.Net.Amount())
	}
	if got := f.balance(chart.Clearing); got != 6000 {
		t.Errorf("clearing = %d, want 6000", got)
	}
	if got := f.balance(chart.Holds); got != 0 {
		t.Errorf("holds = %d, want 0; an authorization is consumed once", got)
	}
}

func TestCaptureRejectsMoreThanAuthorized(t *testing.T) {
	f := newFixture(t, stripeish, true)
	auth := f.charge(10000, false)

	amount := usd(10001)
	if _, err := f.svc.Capture(f.ctx, merchant, auth.ID, &amount); !errors.Is(err, ledger.ErrCaptureExceedsAuthorization) {
		t.Errorf("Capture = %v, want ErrCaptureExceedsAuthorization", err)
	}
}

func TestCaptureRejectsAnAlreadyCapturedPayment(t *testing.T) {
	f := newFixture(t, stripeish, true)
	auth := f.charge(10000, false)

	if _, err := f.svc.Capture(f.ctx, merchant, auth.ID, nil); err != nil {
		t.Fatalf("first Capture = %v", err)
	}
	if _, err := f.svc.Capture(f.ctx, merchant, auth.ID, nil); !errors.Is(err, ErrNotCapturable) {
		t.Errorf("second Capture = %v, want ErrNotCapturable", err)
	}
}

// A cancellation leaves no posted entry at all, which is what distinguishes it from a
// refund in the books.
func TestCancelLeavesNothingPosted(t *testing.T) {
	f := newFixture(t, stripeish, true)
	chart := f.chart()

	auth := f.charge(10000, false)
	canceled, err := f.svc.Cancel(f.ctx, merchant, auth.ID)
	if err != nil {
		t.Fatalf("Cancel = %v", err)
	}

	if canceled.State != StateCanceled {
		t.Errorf("state = %q, want canceled", canceled.State)
	}
	for name, accountID := range map[string]string{
		"clearing": chart.Clearing, "payable": chart.Payable,
		"fee_revenue": chart.FeeRevenue, "holds": chart.Holds,
	} {
		if got := f.balance(accountID); got != 0 {
			t.Errorf("%s = %d after a cancellation, want 0", name, got)
		}
	}
}

func TestCancelRejectsACapturedPayment(t *testing.T) {
	f := newFixture(t, stripeish, true)
	p := f.charge(10000, true)

	if _, err := f.svc.Cancel(f.ctx, merchant, p.ID); !errors.Is(err, ErrNotCapturable) {
		t.Errorf("Cancel = %v, want ErrNotCapturable", err)
	}
}

// ── refunds ─────────────────────────────────────────────────────────────────

func TestFullRefund(t *testing.T) {
	f := newFixture(t, stripeish, true)
	chart := f.chart()

	p := f.charge(10000, true)

	refunded, err := f.svc.Refund(f.ctx, RefundRequest{
		MerchantID: merchant, PaymentID: p.ID, Reason: "customer disputed",
	})
	if err != nil {
		t.Fatalf("Refund = %v", err)
	}

	if refunded.State != StateRefunded {
		t.Errorf("state = %q, want refunded", refunded.State)
	}
	if refunded.Refunded.Amount() != 10000 {
		t.Errorf("refunded = %d, want 10000", refunded.Refunded.Amount())
	}

	// The money left the platform in full.
	if got := f.balance(chart.Clearing); got != 0 {
		t.Errorf("clearing = %d, want 0", got)
	}
	// The percentage fee came back; the fixed 30c did not, so the merchant carries it.
	if got := f.balance(chart.FeeRevenue); got != 30 {
		t.Errorf("fee_revenue = %d, want 30 (the retained fixed component)", got)
	}
	if got := f.balance(chart.Payable); got != -30 {
		t.Errorf("payable = %d, want -30 (the merchant bears the retained fee)", got)
	}
}

func TestPartialRefunds(t *testing.T) {
	f := newFixture(t, stripeish, true)
	p := f.charge(10000, true)

	first := usd(4000)
	after, err := f.svc.Refund(f.ctx, RefundRequest{MerchantID: merchant, PaymentID: p.ID, Amount: &first})
	if err != nil {
		t.Fatalf("first Refund = %v", err)
	}
	if after.State != StatePartiallyRefunded {
		t.Errorf("state = %q, want partially_refunded", after.State)
	}
	if after.Refundable().Amount() != 6000 {
		t.Errorf("refundable = %d, want 6000", after.Refundable().Amount())
	}

	second := usd(6000)
	final, err := f.svc.Refund(f.ctx, RefundRequest{MerchantID: merchant, PaymentID: p.ID, Amount: &second})
	if err != nil {
		t.Fatalf("second Refund = %v", err)
	}
	if final.State != StateRefunded {
		t.Errorf("state = %q, want refunded", final.State)
	}
	if final.Refunded.Amount() != 10000 {
		t.Errorf("refunded = %d, want 10000", final.Refunded.Amount())
	}
	if !final.Refundable().IsZero() {
		t.Errorf("refundable = %d, want 0", final.Refundable().Amount())
	}
}

// Several partial refunds must not together exceed the payment.
func TestRefundsCannotExceedThePayment(t *testing.T) {
	f := newFixture(t, stripeish, true)
	p := f.charge(10000, true)

	most := usd(9000)
	if _, err := f.svc.Refund(f.ctx, RefundRequest{MerchantID: merchant, PaymentID: p.ID, Amount: &most}); err != nil {
		t.Fatalf("Refund = %v", err)
	}

	tooMuch := usd(1001)
	if _, err := f.svc.Refund(f.ctx, RefundRequest{
		MerchantID: merchant, PaymentID: p.ID, Amount: &tooMuch,
	}); !errors.Is(err, ErrRefundExceedsPayment) {
		t.Errorf("Refund = %v, want ErrRefundExceedsPayment", err)
	}

	// The remaining 1000 is still refundable.
	exact := usd(1000)
	if _, err := f.svc.Refund(f.ctx, RefundRequest{MerchantID: merchant, PaymentID: p.ID, Amount: &exact}); err != nil {
		t.Errorf("refunding the exact remainder = %v", err)
	}
}

func TestRefundRejectsUncapturedPayments(t *testing.T) {
	f := newFixture(t, stripeish, true)
	auth := f.charge(10000, false)

	if _, err := f.svc.Refund(f.ctx, RefundRequest{
		MerchantID: merchant, PaymentID: auth.ID,
	}); !errors.Is(err, ErrNotRefundable) {
		t.Errorf("Refund of an authorization = %v, want ErrNotRefundable", err)
	}
}

func TestRefundOfARefundedPaymentIsRejected(t *testing.T) {
	f := newFixture(t, stripeish, true)
	p := f.charge(10000, true)

	if _, err := f.svc.Refund(f.ctx, RefundRequest{MerchantID: merchant, PaymentID: p.ID}); err != nil {
		t.Fatalf("Refund = %v", err)
	}
	if _, err := f.svc.Refund(f.ctx, RefundRequest{
		MerchantID: merchant, PaymentID: p.ID,
	}); !errors.Is(err, ErrNotRefundable) {
		t.Errorf("second full Refund = %v, want ErrNotRefundable", err)
	}
}

// ── derived state ───────────────────────────────────────────────────────────

// Nothing about a payment is stored beside the ledger, so the reported state can never
// contradict the entries. Re-reading it must give the same answer every time.
func TestStateIsDerivedAndStable(t *testing.T) {
	f := newFixture(t, stripeish, true)
	p := f.charge(10000, true)

	for i := 0; i < 3; i++ {
		got, err := f.svc.Get(f.ctx, merchant, p.ID)
		if err != nil {
			t.Fatalf("Get = %v", err)
		}
		if got.State != StateSucceeded || got.Amount.Amount() != 10000 || got.Fee.Amount() != 320 {
			t.Fatalf("read %d gave state=%q amount=%d fee=%d", i, got.State, got.Amount.Amount(), got.Fee.Amount())
		}
	}
}

func TestGetUnknownPayment(t *testing.T) {
	f := newFixture(t, stripeish, true)

	if _, err := f.svc.Get(f.ctx, merchant, "pay_missing"); !errors.Is(err, ErrPaymentNotFound) {
		t.Errorf("Get = %v, want ErrPaymentNotFound", err)
	}
}

// One merchant must not be able to read another's payment.
func TestPaymentsAreScopedToMerchant(t *testing.T) {
	f := newFixture(t, stripeish, true)
	p := f.charge(10000, true)

	if _, err := f.svc.Get(f.ctx, "mer_other", p.ID); !errors.Is(err, ErrPaymentNotFound) {
		t.Errorf("cross-merchant Get = %v, want ErrPaymentNotFound", err)
	}
}

// ── the chart of accounts ───────────────────────────────────────────────────

func TestEnsureChartIsIdempotent(t *testing.T) {
	f := newFixture(t, stripeish, true)

	first, err := EnsureChart(f.ctx, f.l, merchant, money.USD)
	if err != nil {
		t.Fatalf("EnsureChart = %v", err)
	}
	second, err := EnsureChart(f.ctx, f.l, merchant, money.USD)
	if err != nil {
		t.Fatalf("second EnsureChart = %v", err)
	}

	if *first != *second {
		t.Errorf("EnsureChart returned different accounts on the second call:\n%+v\n%+v", first, second)
	}
	if !first.Valid() {
		t.Error("the chart is incomplete")
	}

	accounts, err := f.l.ListAccounts(f.ctx, merchant)
	if err != nil {
		t.Fatalf("ListAccounts = %v", err)
	}
	if len(accounts) != len(chartSpecs) {
		t.Errorf("got %d accounts, want %d; EnsureChart created duplicates", len(accounts), len(chartSpecs))
	}
}

func TestEnsureChartRejectsUnknownCurrency(t *testing.T) {
	f := newFixture(t, stripeish, true)

	if _, err := EnsureChart(f.ctx, f.l, merchant, "XYZ"); !errors.Is(err, money.ErrUnknownCurrency) {
		t.Errorf("EnsureChart = %v, want ErrUnknownCurrency", err)
	}
}

// Charts are per currency, so a JPY payment does not post into USD accounts.
func TestChartsAreSeparatePerCurrency(t *testing.T) {
	f := newFixture(t, stripeish, true)

	usdChart, err := EnsureChart(f.ctx, f.l, merchant, money.USD)
	if err != nil {
		t.Fatalf("EnsureChart(USD) = %v", err)
	}
	jpyChart, err := EnsureChart(f.ctx, f.l, merchant, money.JPY)
	if err != nil {
		t.Fatalf("EnsureChart(JPY) = %v", err)
	}

	if usdChart.Clearing == jpyChart.Clearing {
		t.Error("the USD and JPY charts share a clearing account")
	}
}

// Every payment path must leave the books balanced, which is the invariant everything
// else exists to protect.
func TestEveryPaymentPathLeavesTheBooksBalanced(t *testing.T) {
	f := newFixture(t, stripeish, true)

	// A charge, an authorization captured in part, one cancelled, and a refund.
	f.charge(10000, true)

	auth := f.charge(20000, false)
	half := usd(9000)
	if _, err := f.svc.Capture(f.ctx, merchant, auth.ID, &half); err != nil {
		t.Fatalf("Capture = %v", err)
	}

	cancelled := f.charge(5000, false)
	if _, err := f.svc.Cancel(f.ctx, merchant, cancelled.ID); err != nil {
		t.Fatalf("Cancel = %v", err)
	}

	refundMe := f.charge(7500, true)
	part := usd(2500)
	if _, err := f.svc.Refund(f.ctx, RefundRequest{
		MerchantID: merchant, PaymentID: refundMe.ID, Amount: &part,
	}); err != nil {
		t.Fatalf("Refund = %v", err)
	}

	// Trial balance: every posted balance restated debit-positive must sum to zero.
	accounts, err := f.l.ListAccounts(f.ctx, merchant)
	if err != nil {
		t.Fatalf("ListAccounts = %v", err)
	}

	var trial int64
	for _, a := range accounts {
		trial += a.Type.EquationSign() * f.balance(a.ID)
	}
	if trial != 0 {
		t.Errorf("trial balance = %d, want 0", trial)
	}

	// And the materialized balances still agree with the entry log.
	for _, a := range accounts {
		if got, want := f.balance(a.ID), f.store.DerivedBalance(a.ID); got != want {
			t.Errorf("account %s: materialized %d, derived %d", a.Name, got, want)
		}
	}
}
