package payments

import (
	"context"
	"fmt"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/money"
	"github.com/Madhav-000-s/ledgerd/internal/platform/id"
)

// State is a payment's public lifecycle.
//
// It is derived from ledger facts every time it is read, never stored in a column of its
// own. A stored status is a second source of truth, and the moment it disagrees with the
// entries there is no way to tell which one is lying.
type State string

const (
	StateRequiresCapture   State = "requires_capture"
	StateSucceeded         State = "succeeded"
	StatePartiallyRefunded State = "partially_refunded"
	StateRefunded          State = "refunded"
	StateCanceled          State = "canceled"
)

// Metadata keys used to label a transaction's part in a payment. Deriving state means
// reading these back, so they are constants rather than inline strings.
const (
	metaRole      = "payment_role"
	roleCharge    = "charge"
	roleAuthorize = "authorization"
	roleCapture   = "capture"
	roleVoid      = "void"
	roleRefund    = "refund"
)

// Payment is the public view of a payment, assembled from the ledger.
type Payment struct {
	ID         string
	MerchantID string
	State      State
	Currency   money.Currency

	// Amount is what the customer was charged, or authorized.
	Amount money.Money
	// Fee is the platform's cut, net of any returned on refund.
	Fee money.Money
	// Net is what the merchant is owed, net of refunds.
	Net money.Money
	// Refunded is the cumulative amount refunded.
	Refunded money.Money

	AuthorizationID string
	ChargeID        string
	RefundIDs       []string

	CreatedAt time.Time
}

// Refundable is how much of the payment may still be refunded.
func (p *Payment) Refundable() money.Money {
	remaining := p.Amount.Amount() - p.Refunded.Amount()
	if remaining < 0 {
		remaining = 0
	}
	return money.MustNew(remaining, p.Currency)
}

// Service orchestrates payments over the ledger.
type Service struct {
	ledger ledger.Service
	fees   FeeSchedule
	// refundPercentageFee reports whether the percentage component of a fee comes back
	// on a refund. The fixed component is always retained.
	refundPercentageFee bool
	now                 func() time.Time
}

// Options configures the service.
type Options struct {
	Fees                FeeSchedule
	RefundPercentageFee bool
	Now                 func() time.Time
}

// NewService returns a payments service over the ledger.
func NewService(l ledger.Service, opts Options) (*Service, error) {
	if err := opts.Fees.Validate(); err != nil {
		return nil, err
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{
		ledger:              l,
		fees:                opts.Fees,
		refundPercentageFee: opts.RefundPercentageFee,
		now:                 now,
	}, nil
}

// ChargeRequest asks for a payment.
type ChargeRequest struct {
	MerchantID string
	Amount     money.Money
	// Capture false makes this an authorization, to be captured or cancelled later.
	Capture     bool
	Description string
	Metadata    map[string]any
}

// Charge creates a payment.
//
// With Capture set, it posts the movement described in DESIGN.md §5.1: the platform
// takes the full amount into clearing, owes the merchant the net, and books the fee as
// revenue. Every payment is one balanced transaction, so there is no intermediate state
// in which the fee has been booked but the payable has not.
//
//	DR platform_clearing   amount
//	CR merchant_payable    net
//	CR fee_revenue         fee
//
// Without it, the amount is held pending against the authorization contra account, so
// availability drops immediately while nothing is posted.
func (s *Service) Charge(ctx context.Context, req ChargeRequest) (*Payment, error) {
	if req.Amount.Amount() <= 0 {
		return nil, fmt.Errorf("%w: got %d", ErrAmountRequired, req.Amount.Amount())
	}

	chart, err := EnsureChart(ctx, s.ledger, req.MerchantID, req.Amount.Currency())
	if err != nil {
		return nil, err
	}

	// The fee is computed before anything is written, so a schedule that would produce
	// a fee at or above the amount fails the request rather than posting a negative net.
	fee, net, err := s.fees.Compute(req.Amount)
	if err != nil {
		return nil, err
	}

	paymentID := id.New(id.PrefixPayment)
	// The fee account is recorded on the transaction so that reading a payment back can
	// tell the fee entry from the payable entry without another account lookup per
	// entry — and so that a payment made under an older chart still reads correctly.
	metadata := mergeMetadata(req.Metadata, map[string]any{
		metaRole:       roleCharge,
		metaFeeAccount: chart.FeeRevenue,
	})

	var spec ledger.TransactionSpec
	if req.Capture {
		spec = ledger.TransactionSpec{
			MerchantID:  req.MerchantID,
			Description: descriptionOr(req.Description, "charge"),
			ExternalRef: paymentID,
			Metadata:    metadata,
			Entries: []ledger.EntrySpec{
				{AccountID: chart.Clearing, Direction: ledger.Debit, Amount: req.Amount},
				{AccountID: chart.Payable, Direction: ledger.Credit, Amount: net},
				{AccountID: chart.FeeRevenue, Direction: ledger.Credit, Amount: fee},
			},
		}
	} else {
		metadata[metaRole] = roleAuthorize
		spec = ledger.TransactionSpec{
			MerchantID:  req.MerchantID,
			Description: descriptionOr(req.Description, "authorization"),
			ExternalRef: paymentID,
			Pending:     true,
			Metadata:    metadata,
			Entries: []ledger.EntrySpec{
				{AccountID: chart.Clearing, Direction: ledger.Debit, Amount: req.Amount},
				{AccountID: chart.Holds, Direction: ledger.Credit, Amount: req.Amount},
			},
		}
	}

	if _, err := s.ledger.Post(ctx, spec); err != nil {
		return nil, err
	}

	return s.Get(ctx, req.MerchantID, paymentID)
}

// Capture converts an authorization into a real movement, for the full amount or less.
//
// The hold is released and the charge is posted in the caller's single transaction, so
// there is no instant at which the hold is gone but the money has not arrived. A partial
// capture releases the whole hold and posts less than it: an authorization is consumed
// once, not drawn down repeatedly.
//
// The captured amount carries its own fee, computed on what was actually taken rather
// than on what was authorized.
func (s *Service) Capture(ctx context.Context, merchantID, paymentID string, amount *money.Money) (*Payment, error) {
	payment, err := s.Get(ctx, merchantID, paymentID)
	if err != nil {
		return nil, err
	}
	if payment.State != StateRequiresCapture {
		return nil, fmt.Errorf("%w: %s is %s", ErrNotCapturable, paymentID, payment.State)
	}

	capture := payment.Amount
	if amount != nil {
		if amount.Currency() != payment.Currency {
			return nil, fmt.Errorf("%w: payment is %s, capture is %s",
				ledger.ErrMultiCurrency, payment.Currency, amount.Currency())
		}
		if amount.Amount() <= 0 {
			return nil, fmt.Errorf("%w: got %d", ErrAmountRequired, amount.Amount())
		}
		if amount.Amount() > payment.Amount.Amount() {
			return nil, fmt.Errorf("%w: authorized %d, requested %d",
				ledger.ErrCaptureExceedsAuthorization, payment.Amount.Amount(), amount.Amount())
		}
		capture = *amount
	}

	chart, err := EnsureChart(ctx, s.ledger, merchantID, payment.Currency)
	if err != nil {
		return nil, err
	}

	fee, net, err := s.fees.Compute(capture)
	if err != nil {
		return nil, err
	}

	// Release the hold in full. Void posts the compensating pending entries; it never
	// edits the original, so the authorization stays in the record.
	if _, err := s.ledger.Void(ctx, payment.AuthorizationID); err != nil {
		return nil, err
	}

	if _, err := s.ledger.Post(ctx, ledger.TransactionSpec{
		MerchantID:  merchantID,
		Description: "capture of " + paymentID,
		ExternalRef: paymentID,
		Metadata: map[string]any{
			metaRole:       roleCapture,
			metaFeeAccount: chart.FeeRevenue,
			"captures":     payment.AuthorizationID,
		},
		Entries: []ledger.EntrySpec{
			{AccountID: chart.Clearing, Direction: ledger.Debit, Amount: capture},
			{AccountID: chart.Payable, Direction: ledger.Credit, Amount: net},
			{AccountID: chart.FeeRevenue, Direction: ledger.Credit, Amount: fee},
		},
	}); err != nil {
		return nil, err
	}

	return s.Get(ctx, merchantID, paymentID)
}

// Cancel releases an authorization without capturing it. No posted entry ever exists for
// the payment, which is what distinguishes a cancellation from a refund in the books.
func (s *Service) Cancel(ctx context.Context, merchantID, paymentID string) (*Payment, error) {
	payment, err := s.Get(ctx, merchantID, paymentID)
	if err != nil {
		return nil, err
	}
	if payment.State != StateRequiresCapture {
		return nil, fmt.Errorf("%w: %s is %s", ErrNotCapturable, paymentID, payment.State)
	}

	if _, err := s.ledger.Void(ctx, payment.AuthorizationID); err != nil {
		return nil, err
	}
	return s.Get(ctx, merchantID, paymentID)
}

// RefundRequest asks to return part or all of a captured payment.
type RefundRequest struct {
	MerchantID string
	PaymentID  string
	// Amount nil refunds everything still refundable.
	Amount   *money.Money
	Reason   string
	Metadata map[string]any
}

// Refund returns money to the customer.
//
// A refund is a new transaction that mirrors the charge for the refunded portion, never
// an edit of the original:
//
//	DR merchant_payable   refund − returned fee
//	DR fee_revenue        returned fee
//	CR platform_clearing  refund
//
// The cumulative refunded amount is checked against the charge before anything is
// written, so several partial refunds cannot together exceed the payment.
//
// A note on DESIGN.md §5.3: it describes a full refund as ledger.Reverse on the capture.
// That conflicts with the fee policy stated two sentences later, because a reversal
// necessarily returns the entire fee including the fixed component. Both full and partial
// refunds are therefore mirror transactions, so the fee policy applies uniformly instead
// of depending on whether the refund happened to be for the whole amount.
func (s *Service) Refund(ctx context.Context, req RefundRequest) (*Payment, error) {
	payment, err := s.Get(ctx, req.MerchantID, req.PaymentID)
	if err != nil {
		return nil, err
	}

	switch payment.State {
	case StateSucceeded, StatePartiallyRefunded:
	default:
		return nil, fmt.Errorf("%w: %s is %s", ErrNotRefundable, req.PaymentID, payment.State)
	}

	refundable := payment.Refundable()
	refund := refundable
	if req.Amount != nil {
		if req.Amount.Currency() != payment.Currency {
			return nil, fmt.Errorf("%w: payment is %s, refund is %s",
				ledger.ErrMultiCurrency, payment.Currency, req.Amount.Currency())
		}
		if req.Amount.Amount() <= 0 {
			return nil, fmt.Errorf("%w: got %d", ErrAmountRequired, req.Amount.Amount())
		}
		refund = *req.Amount
	}
	if refund.Amount() > refundable.Amount() {
		return nil, fmt.Errorf("%w: %d refundable, requested %d",
			ErrRefundExceedsPayment, refundable.Amount(), refund.Amount())
	}
	if refund.IsZero() {
		return nil, fmt.Errorf("%w: nothing left to refund", ErrNotRefundable)
	}

	chart, err := EnsureChart(ctx, s.ledger, req.MerchantID, payment.Currency)
	if err != nil {
		return nil, err
	}

	// The already-refunded total is required: rounding each partial refund on its own
	// accumulates error and can return more fee than was ever charged.
	returnedFee, err := s.fees.RefundFee(payment.Amount, payment.Refunded, refund, s.refundPercentageFee)
	if err != nil {
		return nil, err
	}

	fromPayable, err := refund.Sub(returnedFee)
	if err != nil {
		return nil, err
	}

	entries := []ledger.EntrySpec{
		{AccountID: chart.Clearing, Direction: ledger.Credit, Amount: refund},
	}
	if !fromPayable.IsZero() {
		entries = append(entries, ledger.EntrySpec{
			AccountID: chart.Payable, Direction: ledger.Debit, Amount: fromPayable,
		})
	}
	if !returnedFee.IsZero() {
		entries = append(entries, ledger.EntrySpec{
			AccountID: chart.FeeRevenue, Direction: ledger.Debit, Amount: returnedFee,
		})
	}

	metadata := mergeMetadata(req.Metadata, map[string]any{
		metaRole:       roleRefund,
		metaFeeAccount: chart.FeeRevenue,
		"refunds":      req.PaymentID,
		"reason":       req.Reason,
	})

	if _, err := s.ledger.Post(ctx, ledger.TransactionSpec{
		MerchantID:  req.MerchantID,
		Description: "refund of " + req.PaymentID,
		ExternalRef: req.PaymentID,
		Metadata:    metadata,
		Entries:     entries,
	}); err != nil {
		return nil, err
	}

	return s.Get(ctx, req.MerchantID, req.PaymentID)
}

// Get assembles a payment from its ledger transactions.
//
// Everything reported here — the state, the amounts, the refunded total — is computed
// from the entries. There is no stored status that could drift, which is why a payment
// can never claim to be refunded while the books say otherwise.
func (s *Service) Get(ctx context.Context, merchantID, paymentID string) (*Payment, error) {
	txns, err := s.ledger.TransactionsByRef(ctx, merchantID, paymentID)
	if err != nil {
		return nil, err
	}
	if len(txns) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrPaymentNotFound, paymentID)
	}
	return derive(paymentID, merchantID, txns)
}

// derive reconstructs a payment's public shape from its transactions.
func derive(paymentID, merchantID string, txns []*ledger.Transaction) (*Payment, error) {
	p := &Payment{
		ID:         paymentID,
		MerchantID: merchantID,
		Currency:   txns[0].Currency,
		CreatedAt:  txns[0].CreatedAt,
	}

	var (
		amount    int64
		fee       int64
		net       int64
		refunded  int64
		authorize *ledger.Transaction
		charge    *ledger.Transaction
		voided    bool
	)

	for _, t := range txns {
		role, _ := t.Metadata[metaRole].(string)

		switch role {
		case roleAuthorize:
			authorize = t
			amount = t.Total()

		case roleCharge, roleCapture:
			charge = t
			amount = t.Total()
			fee, net = splitChargeTotals(t)

		case roleRefund:
			refunded += t.Total()
			refundedFee, refundedNet := splitRefundTotals(t)
			fee -= refundedFee
			net -= refundedNet
			p.RefundIDs = append(p.RefundIDs, t.ID)

		case roleVoid:
			voided = true
		}

		// A released authorization is recorded by the ledger as a status change on the
		// original rather than a role of its own, so check that too.
		if t.Status == ledger.StatusVoided {
			voided = true
		}
	}

	p.Amount = money.MustNew(amount, p.Currency)
	p.Fee = money.MustNew(fee, p.Currency)
	p.Net = money.MustNew(net, p.Currency)
	p.Refunded = money.MustNew(refunded, p.Currency)

	if authorize != nil {
		p.AuthorizationID = authorize.ID
	}
	if charge != nil {
		p.ChargeID = charge.ID
	}

	switch {
	case charge == nil && voided:
		p.State = StateCanceled
	case charge == nil:
		p.State = StateRequiresCapture
	case refunded >= amount && amount > 0:
		p.State = StateRefunded
	case refunded > 0:
		p.State = StatePartiallyRefunded
	default:
		p.State = StateSucceeded
	}

	return p, nil
}

// splitChargeTotals reads the fee and net out of a charge's own entries, rather than
// recomputing them from the schedule. A payment made under an older schedule must report
// the fee it was actually charged.
func splitChargeTotals(t *ledger.Transaction) (fee, net int64) {
	for _, e := range t.Entries {
		if e.Direction != ledger.Credit {
			continue
		}
		if isFeeEntry(t, e) {
			fee += e.Amount
		} else {
			net += e.Amount
		}
	}
	return fee, net
}

func splitRefundTotals(t *ledger.Transaction) (fee, net int64) {
	for _, e := range t.Entries {
		if e.Direction != ledger.Debit {
			continue
		}
		if isFeeEntry(t, e) {
			fee += e.Amount
		} else {
			net += e.Amount
		}
	}
	return fee, net
}

// feeAccountIDs is recorded on each transaction so that reading it back does not require
// another account lookup per entry.
const metaFeeAccount = "fee_account_id"

func isFeeEntry(t *ledger.Transaction, e ledger.Entry) bool {
	if want, ok := t.Metadata[metaFeeAccount].(string); ok {
		return e.AccountID == want
	}
	return false
}

func descriptionOr(given, fallback string) string {
	if given != "" {
		return given
	}
	return fallback
}

func mergeMetadata(base, extra map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}
