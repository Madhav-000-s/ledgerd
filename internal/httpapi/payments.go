package httpapi

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/Madhav-000-s/ledgerd/internal/money"
	"github.com/Madhav-000-s/ledgerd/internal/payments"
)

// paymentResponse is the wire form of a payment.
//
// Every figure here is derived from the ledger when the payment is read, so the response
// cannot claim a state the books disagree with.
type paymentResponse struct {
	Object          string   `json:"object"`
	ID              string   `json:"id"`
	State           string   `json:"state"`
	Currency        string   `json:"currency"`
	Amount          int64    `json:"amount"`
	Fee             int64    `json:"fee"`
	Net             int64    `json:"net"`
	Refunded        int64    `json:"refunded"`
	Refundable      int64    `json:"refundable"`
	AuthorizationID string   `json:"authorization_id,omitempty"`
	ChargeID        string   `json:"charge_id,omitempty"`
	RefundIDs       []string `json:"refund_ids"`
	CreatedAt       string   `json:"created_at"`
}

func newPaymentResponse(p *payments.Payment) paymentResponse {
	refunds := p.RefundIDs
	if refunds == nil {
		refunds = []string{}
	}
	return paymentResponse{
		Object:          "payment",
		ID:              p.ID,
		State:           string(p.State),
		Currency:        string(p.Currency),
		Amount:          p.Amount.Amount(),
		Fee:             p.Fee.Amount(),
		Net:             p.Net.Amount(),
		Refunded:        p.Refunded.Amount(),
		Refundable:      p.Refundable().Amount(),
		AuthorizationID: p.AuthorizationID,
		ChargeID:        p.ChargeID,
		RefundIDs:       refunds,
		CreatedAt:       p.CreatedAt.UTC().Format(rfc3339Milli),
	}
}

type createPaymentRequest struct {
	Amount      int64          `json:"amount"`
	Currency    string         `json:"currency"`
	Capture     *bool          `json:"capture"`
	Description string         `json:"description"`
	Metadata    map[string]any `json:"metadata"`
}

// handleCreatePayment charges a customer, or authorizes a charge for later capture.
func (s *Server) handleCreatePayment(w http.ResponseWriter, r *http.Request) {
	auth, _ := AuthFrom(r.Context())

	var req createPaymentRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}

	currency, err := money.ParseCurrency(req.Currency)
	if err != nil {
		writeError(w, r, err)
		return
	}
	amount, err := money.New(req.Amount, currency)
	if err != nil {
		writeError(w, r, err)
		return
	}

	// Capture defaults to true: a bare POST /v1/payments is a charge, and an
	// authorization is the case a caller opts into.
	capture := true
	if req.Capture != nil {
		capture = *req.Capture
	}

	var body []byte
	err = s.tx.InTx(r.Context(), func(ctx context.Context) error {
		payment, err := s.payments.Charge(ctx, payments.ChargeRequest{
			MerchantID:  auth.MerchantID,
			Amount:      amount,
			Capture:     capture,
			Description: req.Description,
			Metadata:    req.Metadata,
		})
		if err != nil {
			return err
		}
		body, err = s.completeIdempotent(ctx, http.StatusCreated, newPaymentResponse(payment), payment.ID)
		return err
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeRecorded(w, http.StatusCreated, body)
}

type capturePaymentRequest struct {
	Amount *int64 `json:"amount"`
}

// handleCapturePayment converts an authorization into a real movement.
func (s *Server) handleCapturePayment(w http.ResponseWriter, r *http.Request) {
	auth, _ := AuthFrom(r.Context())
	paymentID := chi.URLParam(r, "id")

	var req capturePaymentRequest
	if r.ContentLength > 0 {
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, r, err)
			return
		}
	}

	var body []byte
	err := s.tx.InTx(r.Context(), func(ctx context.Context) error {
		amount, err := s.optionalAmount(ctx, auth.MerchantID, paymentID, req.Amount)
		if err != nil {
			return err
		}

		payment, err := s.payments.Capture(ctx, auth.MerchantID, paymentID, amount)
		if err != nil {
			return err
		}
		body, err = s.completeIdempotent(ctx, http.StatusOK, newPaymentResponse(payment), payment.ID)
		return err
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeRecorded(w, http.StatusOK, body)
}

// handleCancelPayment releases an authorization without capturing it.
func (s *Server) handleCancelPayment(w http.ResponseWriter, r *http.Request) {
	auth, _ := AuthFrom(r.Context())
	paymentID := chi.URLParam(r, "id")

	var body []byte
	err := s.tx.InTx(r.Context(), func(ctx context.Context) error {
		payment, err := s.payments.Cancel(ctx, auth.MerchantID, paymentID)
		if err != nil {
			return err
		}
		body, err = s.completeIdempotent(ctx, http.StatusOK, newPaymentResponse(payment), payment.ID)
		return err
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeRecorded(w, http.StatusOK, body)
}

func (s *Server) handleGetPayment(w http.ResponseWriter, r *http.Request) {
	auth, _ := AuthFrom(r.Context())

	payment, err := s.payments.Get(r.Context(), auth.MerchantID, chi.URLParam(r, "id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, r, http.StatusOK, newPaymentResponse(payment))
}

type createRefundRequest struct {
	PaymentID string         `json:"payment_id"`
	Amount    *int64         `json:"amount"`
	Reason    string         `json:"reason"`
	Metadata  map[string]any `json:"metadata"`
}

// handleCreateRefund returns money to the customer.
//
// A refund is a new transaction, never an edit of the original, so both the charge and
// its correction stay permanent facts in the books.
func (s *Server) handleCreateRefund(w http.ResponseWriter, r *http.Request) {
	auth, _ := AuthFrom(r.Context())

	var req createRefundRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, r, err)
		return
	}
	if req.PaymentID == "" {
		writeError(w, r, Errorf(http.StatusBadRequest, TypeInvalidRequest, "payment_id_required",
			"payment_id is required.").WithParam("payment_id"))
		return
	}

	var body []byte
	err := s.tx.InTx(r.Context(), func(ctx context.Context) error {
		amount, err := s.optionalAmount(ctx, auth.MerchantID, req.PaymentID, req.Amount)
		if err != nil {
			return err
		}

		payment, err := s.payments.Refund(ctx, payments.RefundRequest{
			MerchantID: auth.MerchantID,
			PaymentID:  req.PaymentID,
			Amount:     amount,
			Reason:     req.Reason,
			Metadata:   req.Metadata,
		})
		if err != nil {
			return err
		}
		body, err = s.completeIdempotent(ctx, http.StatusCreated, newPaymentResponse(payment), payment.ID)
		return err
	})
	if err != nil {
		writeError(w, r, err)
		return
	}

	writeRecorded(w, http.StatusCreated, body)
}

// optionalAmount resolves a nullable minor-unit amount against the payment's currency.
//
// The currency is taken from the payment rather than the request. A caller cannot
// capture a USD authorization in EUR by saying so, and there is no second place for the
// two to disagree.
func (s *Server) optionalAmount(ctx context.Context, merchantID, paymentID string, minor *int64) (*money.Money, error) {
	if minor == nil {
		return nil, nil
	}

	payment, err := s.payments.Get(ctx, merchantID, paymentID)
	if err != nil {
		return nil, err
	}

	amount, err := money.New(*minor, payment.Currency)
	if err != nil {
		return nil, err
	}
	return &amount, nil
}
