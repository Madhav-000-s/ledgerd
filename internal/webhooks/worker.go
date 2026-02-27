package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/events"
	"github.com/Madhav-000-s/ledgerd/internal/platform/id"
	"github.com/Madhav-000-s/ledgerd/internal/platform/log"
)

// TxRunner composes several store calls into one database transaction.
type TxRunner interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// Dispatcher expands events into per-endpoint deliveries.
type Dispatcher struct {
	events    events.Store
	webhooks  Store
	tx        TxRunner
	batchSize int
	now       func() time.Time
}

// NewDispatcher builds a dispatcher.
func NewDispatcher(ev events.Store, wh Store, tx TxRunner, batchSize int, now func() time.Time) *Dispatcher {
	if batchSize <= 0 {
		batchSize = 100
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Dispatcher{events: ev, webhooks: wh, tx: tx, batchSize: batchSize, now: now}
}

// DispatchOnce claims a batch of undispatched events, expands each against the
// merchant's matching endpoints, and marks them dispatched — all in one transaction.
//
// Doing the expansion and the mark together is what makes a crashed dispatcher harmless:
// either an event is dispatched and its deliveries exist, or neither is true and the
// next run picks it up again. The unique constraint on (event_id, endpoint_id) makes the
// re-run idempotent even if the crash landed mid-insert.
//
// It returns the number of events dispatched.
func (d *Dispatcher) DispatchOnce(ctx context.Context) (int, error) {
	var dispatched int

	err := d.tx.InTx(ctx, func(ctx context.Context) error {
		batch, err := d.events.ClaimUndispatched(ctx, d.batchSize)
		if err != nil {
			return err
		}
		if len(batch) == 0 {
			return nil
		}

		// Endpoints are looked up once per merchant rather than once per event: a batch
		// is usually many events for a handful of merchants.
		endpointsByMerchant := make(map[string][]*Endpoint)

		var (
			deliveries []*Delivery
			eventIDs   = make([]string, 0, len(batch))
		)

		for _, e := range batch {
			eventIDs = append(eventIDs, e.ID)

			endpoints, ok := endpointsByMerchant[e.MerchantID]
			if !ok {
				if endpoints, err = d.webhooks.ActiveEndpointsForEvent(ctx, e.MerchantID); err != nil {
					return err
				}
				endpointsByMerchant[e.MerchantID] = endpoints
			}

			for _, endpoint := range endpoints {
				if !endpoint.Wants(e.Type) {
					continue
				}
				deliveries = append(deliveries, &Delivery{
					ID:            id.New(id.PrefixDelivery),
					EventID:       e.ID,
					EndpointID:    endpoint.ID,
					MerchantID:    e.MerchantID,
					State:         StatePending,
					NextAttemptAt: d.now(),
					CreatedAt:     d.now(),
				})
			}
		}

		if len(deliveries) > 0 {
			if err := d.webhooks.EnqueueDeliveries(ctx, deliveries); err != nil {
				return err
			}
		}
		// An event with no matching endpoint is still marked dispatched. Leaving it
		// unmarked would make the dispatcher re-examine it forever.
		if err := d.events.MarkDispatched(ctx, eventIDs); err != nil {
			return err
		}

		dispatched = len(batch)
		return nil
	})

	return dispatched, err
}

// Worker delivers queued webhooks.
type Worker struct {
	store    Store
	client   *Client
	signer   *Signer
	cipher   *Cipher
	backoff  Backoff
	tx       TxRunner
	maxTries int
	lease    time.Duration
	batch    int
	now      func() time.Time
}

// WorkerOptions configures a delivery worker.
type WorkerOptions struct {
	Store       Store
	Client      *Client
	Cipher      *Cipher
	Backoff     Backoff
	Tx          TxRunner
	MaxAttempts int
	Lease       time.Duration
	BatchSize   int
	Now         func() time.Time
}

// NewWorker builds a delivery worker.
func NewWorker(opts WorkerOptions) *Worker {
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = MaxAttempts
	}
	if opts.Lease <= 0 {
		opts.Lease = 60 * time.Second
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 50
	}

	return &Worker{
		store:    opts.Store,
		client:   opts.Client,
		signer:   NewSigner(now),
		cipher:   opts.Cipher,
		backoff:  opts.Backoff,
		tx:       opts.Tx,
		maxTries: opts.MaxAttempts,
		lease:    opts.Lease,
		batch:    opts.BatchSize,
		now:      now,
	}
}

// DeliverOnce claims a batch of due deliveries and attempts each.
//
// The claim uses FOR UPDATE SKIP LOCKED, so N workers can run against one queue with no
// coordination between them: each takes rows the others are not holding, and a row is
// never worked twice concurrently.
//
// The HTTP calls happen strictly outside any transaction. Holding a database transaction
// open across a network call to a merchant endpoint — which may take the full five-second
// timeout — is how a worker pool exhausts the connection pool and takes the API down with
// it.
func (w *Worker) DeliverOnce(ctx context.Context) (int, error) {
	claimed, err := w.store.ClaimDue(ctx, w.batch, w.now().Add(w.lease))
	if err != nil {
		return 0, err
	}

	for _, delivery := range claimed {
		if err := w.attempt(ctx, delivery); err != nil {
			log.From(ctx).Error("webhook delivery attempt failed",
				slog.String(log.KeyDeliveryID, delivery.ID), log.Err(err))
		}
	}
	return len(claimed), nil
}

// attempt performs one delivery attempt and records the result.
func (w *Worker) attempt(ctx context.Context, delivery *Delivery) error {
	logger := log.From(ctx).With(
		slog.String(log.KeyDeliveryID, delivery.ID),
		slog.String(log.KeyEndpointID, delivery.EndpointID),
		slog.Int(log.KeyAttempt, delivery.Attempt),
	)

	endpoint, err := w.store.GetEndpoint(ctx, delivery.MerchantID, delivery.EndpointID)
	if err != nil {
		return err
	}
	if endpoint.Status == EndpointDisabled {
		return w.store.CompleteDelivery(ctx, delivery.ID, StateDisabled, Response{})
	}

	event, err := w.store.GetEvent(ctx, delivery.EventID)
	if err != nil {
		return err
	}

	body, err := json.Marshal(event.Envelope())
	if err != nil {
		return err
	}

	secrets, err := w.activeSecrets(ctx, endpoint.ID)
	if err != nil {
		return err
	}

	// Sign the exact bytes that will be sent, and send exactly those bytes.
	header, _, err := w.signer.Sign(body, secrets)
	if err != nil {
		return err
	}

	headers := map[string]string{
		"Webhook-Signature": header,
		"Webhook-Id":        event.ID,
		"Webhook-Topic":     string(event.Type),
		"Webhook-Attempt":   strconv.Itoa(delivery.Attempt + 1),
	}

	resp := w.client.Deliver(ctx, endpoint.URL, body, headers)

	if err := w.store.RecordAttempt(ctx, &Attempt{
		DeliveryID:  delivery.ID,
		Attempt:     delivery.Attempt + 1,
		StatusCode:  statusPtr(resp),
		Error:       errString(resp.Err),
		DurationMS:  durationPtr(resp.Duration),
		AttemptedAt: w.now(),
	}); err != nil {
		return err
	}

	outcome := Classify(resp, delivery.Attempt, w.maxTries)
	logger.Info("webhook delivery attempted",
		slog.String(log.KeyOutcome, outcome.String()),
		slog.Int("status", resp.StatusCode),
		slog.Duration("duration", resp.Duration),
	)

	switch outcome {
	case OutcomeSucceeded:
		if err := w.store.CompleteDelivery(ctx, delivery.ID, StateSucceeded, resp); err != nil {
			return err
		}
		// A success clears the endpoint's failure streak, so an endpoint that recovers
		// is not disabled by history it has already made up for.
		return w.store.ResetEndpointFailures(ctx, endpoint.ID)

	case OutcomeDisabled:
		if err := w.store.CompleteDelivery(ctx, delivery.ID, StateDisabled, resp); err != nil {
			return err
		}
		return w.store.SetEndpointStatus(ctx, endpoint.ID, EndpointDisabled)

	case OutcomeExhausted:
		if err := w.store.CompleteDelivery(ctx, delivery.ID, StateExhausted, resp); err != nil {
			return err
		}
		return w.recordEndpointFailure(ctx, endpoint)

	default:
		return w.store.RescheduleDelivery(ctx, delivery.ID, w.backoff.Delay(delivery.Attempt), resp)
	}
}

// recordEndpointFailure applies the degrade and disable thresholds.
func (w *Worker) recordEndpointFailure(ctx context.Context, endpoint *Endpoint) error {
	failures, err := w.store.RecordEndpointFailure(ctx, endpoint.ID)
	if err != nil {
		return err
	}

	switch {
	case failures >= DisableAfter:
		return w.store.SetEndpointStatus(ctx, endpoint.ID, EndpointDisabled)
	case failures >= DegradeAfter && endpoint.Status == EndpointActive:
		return w.store.SetEndpointStatus(ctx, endpoint.ID, EndpointDegraded)
	default:
		return nil
	}
}

// activeSecrets decrypts an endpoint's signing secrets.
func (w *Worker) activeSecrets(ctx context.Context, endpointID string) ([]*Secret, error) {
	stored, err := w.store.ActiveSecrets(ctx, endpointID)
	if err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return nil, ErrSecretNotFound
	}
	return stored, nil
}

// ReclaimOnce resets deliveries whose worker died mid-POST.
//
// This is the crash-recovery path, and it is exactly why delivery is at-least-once: a
// worker that died after the endpoint accepted the POST but before it recorded the
// result leaves a row that will be delivered again. Consumers are told to dedupe on the
// event id because of this case, not as a formality.
func (w *Worker) ReclaimOnce(ctx context.Context) (int64, error) {
	return w.store.ReclaimExpiredLeases(ctx, w.now())
}

func statusPtr(r Response) *int {
	if r.Err != nil && r.StatusCode == 0 {
		return nil
	}
	code := r.StatusCode
	return &code
}

func durationPtr(d time.Duration) *int {
	ms := int(d.Milliseconds())
	return &ms
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, ErrRedirect) {
		return "endpoint returned a redirect, which is not followed"
	}
	return err.Error()
}
