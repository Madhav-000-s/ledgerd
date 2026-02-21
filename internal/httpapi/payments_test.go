package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/ledger/memory"
	"github.com/Madhav-000-s/ledgerd/internal/payments"
	"github.com/Madhav-000-s/ledgerd/internal/platform/config"
)

// payHarness is the account harness plus a payments service.
type payHarness struct {
	*harness
}

func newPayHarness(t *testing.T) *payHarness {
	t.Helper()

	store := memory.New()
	svc := ledger.NewService(store)

	keys := &fakeKeyStore{byLookup: map[string]*APIKeyRecord{}}
	h := &harness{t: t, store: store, svc: svc, keys: keys}
	h.writeKey = h.issueKey(merchantA, RoleWrite)
	h.readKey = h.issueKey(merchantA, RoleRead)
	h.otherKey = h.issueKey(merchantB, RoleWrite)

	paySvc, err := payments.NewService(svc, payments.Options{
		Fees:                payments.FeeSchedule{Bps: 290, Fixed: 30},
		RefundPercentageFee: true,
	})
	if err != nil {
		t.Fatalf("payments.NewService = %v", err)
	}

	h.server = NewServer(Options{
		Config: config.HTTPConfig{
			HandlerTimeout: 5 * time.Second,
			MaxBodyBytes:   1 << 20,
		},
		Ledger:   svc,
		Tx:       passthroughTx{},
		Keys:     keys,
		Health:   okHealth{},
		Payments: paySvc,
	})
	return &payHarness{harness: h}
}

func TestCreatePayment(t *testing.T) {
	h := newPayHarness(t)

	rec := h.do(http.MethodPost, "/v1/payments", h.writeKey, map[string]any{
		"amount": 10000, "currency": "USD",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body %s", rec.Code, rec.Body.String())
	}

	p := decodeBody[paymentResponse](t, rec)
	if p.Object != "payment" {
		t.Errorf("object = %q, want payment", p.Object)
	}
	if p.State != string(payments.StateSucceeded) {
		t.Errorf("state = %q, want succeeded", p.State)
	}
	if p.Amount != 10000 || p.Fee != 320 || p.Net != 9680 {
		t.Errorf("amount/fee/net = %d/%d/%d, want 10000/320/9680", p.Amount, p.Fee, p.Net)
	}
	if p.Refundable != 10000 {
		t.Errorf("refundable = %d, want 10000", p.Refundable)
	}
}

// A 1c charge under a 2.9% + 30c schedule must be refused with a 422, not accepted and
// left for the books to make sense of.
func TestCreatePaymentRefusesWhenTheFeeExceedsTheAmount(t *testing.T) {
	h := newPayHarness(t)

	rec := h.do(http.MethodPost, "/v1/payments", h.writeKey, map[string]any{
		"amount": 1, "currency": "USD",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body %s", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Code; got != "fee_exceeds_amount" {
		t.Errorf("code = %q, want fee_exceeds_amount", got)
	}
}

func TestAuthorizeCaptureFlow(t *testing.T) {
	h := newPayHarness(t)

	capture := false
	created := decodeBody[paymentResponse](t, h.do(http.MethodPost, "/v1/payments", h.writeKey, map[string]any{
		"amount": 10000, "currency": "USD", "capture": &capture,
	}))
	if created.State != string(payments.StateRequiresCapture) {
		t.Fatalf("state = %q, want requires_capture", created.State)
	}

	rec := h.do(http.MethodPost, "/v1/payments/"+created.ID+"/capture", h.writeKey,
		map[string]any{"amount": 6000})
	if rec.Code != http.StatusOK {
		t.Fatalf("capture status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}

	captured := decodeBody[paymentResponse](t, rec)
	if captured.State != string(payments.StateSucceeded) {
		t.Errorf("state = %q, want succeeded", captured.State)
	}
	if captured.Amount != 6000 {
		t.Errorf("amount = %d, want 6000", captured.Amount)
	}
	// 2.9% of 6000 is 174, plus the fixed 30.
	if captured.Fee != 204 {
		t.Errorf("fee = %d, want 204", captured.Fee)
	}
}

func TestCancelAuthorization(t *testing.T) {
	h := newPayHarness(t)

	capture := false
	created := decodeBody[paymentResponse](t, h.do(http.MethodPost, "/v1/payments", h.writeKey, map[string]any{
		"amount": 10000, "currency": "USD", "capture": &capture,
	}))

	rec := h.do(http.MethodPost, "/v1/payments/"+created.ID+"/cancel", h.writeKey, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if got := decodeBody[paymentResponse](t, rec).State; got != string(payments.StateCanceled) {
		t.Errorf("state = %q, want canceled", got)
	}
}

func TestCaptureAnAlreadyCapturedPaymentConflicts(t *testing.T) {
	h := newPayHarness(t)

	created := decodeBody[paymentResponse](t, h.do(http.MethodPost, "/v1/payments", h.writeKey, map[string]any{
		"amount": 10000, "currency": "USD",
	}))

	rec := h.do(http.MethodPost, "/v1/payments/"+created.ID+"/capture", h.writeKey, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	if got := decodeError(t, rec).Code; got != "payment_not_capturable" {
		t.Errorf("code = %q", got)
	}
}

func TestRefundFlow(t *testing.T) {
	h := newPayHarness(t)

	created := decodeBody[paymentResponse](t, h.do(http.MethodPost, "/v1/payments", h.writeKey, map[string]any{
		"amount": 10000, "currency": "USD",
	}))

	partial := decodeBody[paymentResponse](t, h.do(http.MethodPost, "/v1/refunds", h.writeKey, map[string]any{
		"payment_id": created.ID, "amount": 4000, "reason": "partial return",
	}))
	if partial.State != string(payments.StatePartiallyRefunded) {
		t.Errorf("state = %q, want partially_refunded", partial.State)
	}
	if partial.Refunded != 4000 || partial.Refundable != 6000 {
		t.Errorf("refunded/refundable = %d/%d, want 4000/6000", partial.Refunded, partial.Refundable)
	}

	full := decodeBody[paymentResponse](t, h.do(http.MethodPost, "/v1/refunds", h.writeKey, map[string]any{
		"payment_id": created.ID,
	}))
	if full.State != string(payments.StateRefunded) {
		t.Errorf("state = %q, want refunded", full.State)
	}
	if full.Refundable != 0 {
		t.Errorf("refundable = %d, want 0", full.Refundable)
	}
	if len(full.RefundIDs) != 2 {
		t.Errorf("got %d refund ids, want 2", len(full.RefundIDs))
	}
}

func TestRefundValidation(t *testing.T) {
	h := newPayHarness(t)

	created := decodeBody[paymentResponse](t, h.do(http.MethodPost, "/v1/payments", h.writeKey, map[string]any{
		"amount": 10000, "currency": "USD",
	}))

	tests := []struct {
		name     string
		body     map[string]any
		wantCode string
	}{
		{"no payment id", map[string]any{"amount": 100}, "payment_id_required"},
		{"unknown payment", map[string]any{"payment_id": "pay_missing"}, "payment_not_found"},
		{"too much", map[string]any{"payment_id": created.ID, "amount": 10001}, "refund_exceeds_payment"},
		{"zero", map[string]any{"payment_id": created.ID, "amount": 0}, "invalid_amount"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := h.do(http.MethodPost, "/v1/refunds", h.writeKey, tc.body)
			if rec.Code < 400 || rec.Code >= 500 {
				t.Fatalf("status = %d, want a 4xx; body %s", rec.Code, rec.Body.String())
			}
			if got := decodeError(t, rec).Code; got != tc.wantCode {
				t.Errorf("code = %q, want %q", got, tc.wantCode)
			}
		})
	}
}

// One merchant must not see or act on another's payment, and the failure must be a 404
// rather than a 403 so the endpoint cannot be used to discover payment ids.
func TestPaymentsAreIsolatedBetweenMerchants(t *testing.T) {
	h := newPayHarness(t)

	created := decodeBody[paymentResponse](t, h.do(http.MethodPost, "/v1/payments", h.writeKey, map[string]any{
		"amount": 10000, "currency": "USD",
	}))

	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/v1/payments/" + created.ID, nil},
		{http.MethodPost, "/v1/payments/" + created.ID + "/capture", nil},
		{http.MethodPost, "/v1/payments/" + created.ID + "/cancel", nil},
		{http.MethodPost, "/v1/refunds", map[string]any{"payment_id": created.ID}},
	} {
		rec := h.do(tc.method, tc.path, h.otherKey, tc.body)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s: status = %d, want 404", tc.method, tc.path, rec.Code)
		}
	}
}

func TestGetPayment(t *testing.T) {
	h := newPayHarness(t)

	created := decodeBody[paymentResponse](t, h.do(http.MethodPost, "/v1/payments", h.writeKey, map[string]any{
		"amount": 10000, "currency": "USD",
	}))

	got := decodeBody[paymentResponse](t, h.do(http.MethodGet, "/v1/payments/"+created.ID, h.readKey, nil))
	if got.ID != created.ID || got.Amount != 10000 || got.Fee != 320 {
		t.Errorf("re-reading the payment gave %+v", got)
	}

	rec := h.do(http.MethodGet, "/v1/payments/pay_missing", h.readKey, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown payment: status = %d, want 404", rec.Code)
	}
}

// A payment's state is derived from the ledger, so the books and the API can never
// disagree about what happened.
func TestPaymentStateMatchesTheLedger(t *testing.T) {
	h := newPayHarness(t)

	created := decodeBody[paymentResponse](t, h.do(http.MethodPost, "/v1/payments", h.writeKey, map[string]any{
		"amount": 10000, "currency": "USD",
	}))

	// Every posted balance restated debit-positive must sum to zero.
	accounts := decodeBody[listResponse[accountResponse]](t,
		h.do(http.MethodGet, "/v1/accounts", h.readKey, nil))

	var trial int64
	for _, a := range accounts.Data {
		balance := decodeBody[balanceResponse](t,
			h.do(http.MethodGet, "/v1/accounts/"+a.ID+"/balance", h.readKey, nil))
		trial += ledger.AccountType(a.Type).EquationSign() * balance.Posted
	}
	if trial != 0 {
		t.Errorf("trial balance across the API = %d, want 0", trial)
	}

	if created.Fee+created.Net != created.Amount {
		t.Errorf("fee + net = %d, does not reconstruct the amount %d",
			created.Fee+created.Net, created.Amount)
	}
}
