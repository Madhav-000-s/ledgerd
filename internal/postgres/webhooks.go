package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Madhav-000-s/ledgerd/internal/events"
	"github.com/Madhav-000-s/ledgerd/internal/postgres/sqlc"
	"github.com/Madhav-000-s/ledgerd/internal/webhooks"
)

// EventRepo implements events.Store.
type EventRepo struct{ db *DB }

var _ events.Store = (*EventRepo)(nil)

// NewEventRepo returns an outbox backed by db.
func NewEventRepo(db *DB) *EventRepo { return &EventRepo{db: db} }

func (r *EventRepo) q(ctx context.Context) *sqlc.Queries { return sqlc.New(r.db.q(ctx)) }

// Append writes an event inside the caller's transaction.
//
// The whole outbox rests on this being called from within the same transaction as the
// ledger write. There is deliberately no variant that opens its own.
func (r *EventRepo) Append(ctx context.Context, e *events.Event) error {
	err := r.q(ctx).AppendEvent(ctx, sqlc.AppendEventParams{
		ID:         e.ID,
		MerchantID: e.MerchantID,
		Type:       string(e.Type),
		ResourceID: e.ResourceID,
		Payload:    e.Payload,
		CreatedAt:  timestamptz(e.CreatedAt),
	})
	if err != nil {
		return fmt.Errorf("postgres: append event: %w", err)
	}
	return nil
}

func (r *EventRepo) Get(ctx context.Context, merchantID, eventID string) (*events.Event, error) {
	row, err := r.q(ctx).GetEventForMerchant(ctx, sqlc.GetEventForMerchantParams{
		MerchantID: merchantID,
		ID:         eventID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", events.ErrEventNotFound, eventID)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get event: %w", err)
	}
	return eventFromRow(sqlc.Event(row)), nil
}

func (r *EventRepo) List(ctx context.Context, merchantID string, limit int, before string) ([]*events.Event, string, error) {
	if limit <= 0 {
		limit = 100
	}

	var beforeID *string
	if before != "" {
		beforeID = &before
	}

	// One extra row tells us whether there is a next page.
	rows, err := r.q(ctx).ListEvents(ctx, sqlc.ListEventsParams{
		MerchantID: merchantID,
		BeforeID:   beforeID,
		Limit:      int32(limit) + 1,
	})
	if err != nil {
		return nil, "", fmt.Errorf("postgres: list events: %w", err)
	}

	out := make([]*events.Event, 0, len(rows))
	for _, row := range rows {
		out = append(out, eventFromRow(sqlc.Event(row)))
	}
	if len(out) > limit {
		out = out[:limit]
		return out, out[len(out)-1].ID, nil
	}
	return out, "", nil
}

func (r *EventRepo) ClaimUndispatched(ctx context.Context, limit int) ([]*events.Event, error) {
	rows, err := r.q(ctx).ClaimUndispatchedEvents(ctx, int32(limit))
	if err != nil {
		return nil, fmt.Errorf("postgres: claim undispatched events: %w", err)
	}

	out := make([]*events.Event, 0, len(rows))
	for _, row := range rows {
		out = append(out, eventFromRow(sqlc.Event(row)))
	}
	return out, nil
}

func (r *EventRepo) MarkDispatched(ctx context.Context, eventIDs []string) error {
	if len(eventIDs) == 0 {
		return nil
	}
	if _, err := r.q(ctx).MarkEventsDispatched(ctx, eventIDs); err != nil {
		return fmt.Errorf("postgres: mark events dispatched: %w", err)
	}
	return nil
}

func eventFromRow(row sqlc.Event) *events.Event {
	e := &events.Event{
		ID:         row.ID,
		MerchantID: row.MerchantID,
		Type:       events.Type(row.Type),
		ResourceID: row.ResourceID,
		Payload:    row.Payload,
		CreatedAt:  row.CreatedAt.Time,
	}
	if row.DispatchedAt.Valid {
		at := row.DispatchedAt.Time
		e.DispatchedAt = &at
	}
	return e
}

// ── webhooks ────────────────────────────────────────────────────────────────

// WebhookRepo implements webhooks.Store.
type WebhookRepo struct {
	db     *DB
	cipher *webhooks.Cipher
}

var _ webhooks.Store = (*WebhookRepo)(nil)

// NewWebhookRepo returns a webhook store. The cipher decrypts signing secrets, which the
// worker needs in plaintext to sign with.
func NewWebhookRepo(db *DB, cipher *webhooks.Cipher) *WebhookRepo {
	return &WebhookRepo{db: db, cipher: cipher}
}

func (r *WebhookRepo) q(ctx context.Context) *sqlc.Queries { return sqlc.New(r.db.q(ctx)) }

func (r *WebhookRepo) CreateEndpoint(ctx context.Context, e *webhooks.Endpoint) error {
	err := r.q(ctx).CreateWebhookEndpoint(ctx, sqlc.CreateWebhookEndpointParams{
		ID:            e.ID,
		MerchantID:    e.MerchantID,
		Url:           e.URL,
		EnabledEvents: e.EnabledEvents,
		Description:   e.Description,
		CreatedAt:     timestamptz(e.CreatedAt),
	})
	if err != nil {
		return fmt.Errorf("postgres: create webhook endpoint: %w", err)
	}
	return nil
}

func (r *WebhookRepo) GetEndpoint(ctx context.Context, merchantID, endpointID string) (*webhooks.Endpoint, error) {
	row, err := r.q(ctx).GetWebhookEndpoint(ctx, sqlc.GetWebhookEndpointParams{
		MerchantID: merchantID,
		ID:         endpointID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", webhooks.ErrEndpointNotFound, endpointID)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get webhook endpoint: %w", err)
	}
	return endpointFromRow(sqlc.WebhookEndpoint(row)), nil
}

func (r *WebhookRepo) ListEndpoints(ctx context.Context, merchantID string) ([]*webhooks.Endpoint, error) {
	rows, err := r.q(ctx).ListWebhookEndpoints(ctx, merchantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list webhook endpoints: %w", err)
	}

	out := make([]*webhooks.Endpoint, 0, len(rows))
	for _, row := range rows {
		out = append(out, endpointFromRow(sqlc.WebhookEndpoint(row)))
	}
	return out, nil
}

func (r *WebhookRepo) ActiveEndpointsForEvent(ctx context.Context, merchantID string) ([]*webhooks.Endpoint, error) {
	rows, err := r.q(ctx).ActiveWebhookEndpoints(ctx, merchantID)
	if err != nil {
		return nil, fmt.Errorf("postgres: active webhook endpoints: %w", err)
	}

	out := make([]*webhooks.Endpoint, 0, len(rows))
	for _, row := range rows {
		out = append(out, endpointFromRow(sqlc.WebhookEndpoint(row)))
	}
	return out, nil
}

func (r *WebhookRepo) SetEndpointStatus(ctx context.Context, endpointID string, status webhooks.EndpointStatus) error {
	affected, err := r.q(ctx).SetWebhookEndpointStatus(ctx, sqlc.SetWebhookEndpointStatusParams{
		ID:     endpointID,
		Status: sqlc.EndpointStatus(status),
	})
	if err != nil {
		return fmt.Errorf("postgres: set endpoint status: %w", err)
	}
	if affected == 0 {
		return webhooks.ErrEndpointNotFound
	}
	return nil
}

func (r *WebhookRepo) RecordEndpointFailure(ctx context.Context, endpointID string) (int, error) {
	failures, err := r.q(ctx).IncrementEndpointFailures(ctx, endpointID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, webhooks.ErrEndpointNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("postgres: increment endpoint failures: %w", err)
	}
	return int(failures), nil
}

func (r *WebhookRepo) ResetEndpointFailures(ctx context.Context, endpointID string) error {
	if _, err := r.q(ctx).ResetEndpointFailures(ctx, endpointID); err != nil {
		return fmt.Errorf("postgres: reset endpoint failures: %w", err)
	}
	return nil
}

func (r *WebhookRepo) CreateSecret(ctx context.Context, s *webhooks.Secret, hash, encrypted []byte) error {
	err := r.q(ctx).CreateWebhookSecret(ctx, sqlc.CreateWebhookSecretParams{
		ID:         s.ID,
		EndpointID: s.EndpointID,
		SecretHash: hash,
		SecretEnc:  encrypted,
		CreatedAt:  timestamptz(s.CreatedAt),
	})
	if err != nil {
		return fmt.Errorf("postgres: create webhook secret: %w", err)
	}
	return nil
}

// ActiveSecrets returns an endpoint's signing secrets, decrypted.
//
// Every active secret is returned, not just the newest: during a rotation the payload is
// signed with all of them, which is what lets a consumer move across at its own pace
// rather than during a coordinated cutover.
func (r *WebhookRepo) ActiveSecrets(ctx context.Context, endpointID string) ([]*webhooks.Secret, error) {
	rows, err := r.q(ctx).ActiveWebhookSecrets(ctx, endpointID)
	if err != nil {
		return nil, fmt.Errorf("postgres: active webhook secrets: %w", err)
	}
	if len(rows) == 0 {
		return nil, webhooks.ErrSecretNotFound
	}

	out := make([]*webhooks.Secret, 0, len(rows))
	for _, row := range rows {
		plaintext, err := r.cipher.Decrypt(row.SecretEnc)
		if err != nil {
			return nil, fmt.Errorf("postgres: decrypting secret %s: %w", row.ID, err)
		}
		out = append(out, &webhooks.Secret{
			ID:         row.ID,
			EndpointID: row.EndpointID,
			Plaintext:  plaintext,
			Active:     row.Active,
			CreatedAt:  row.CreatedAt.Time,
		})
	}
	return out, nil
}

func (r *WebhookRepo) RetireSecret(ctx context.Context, secretID string) error {
	affected, err := r.q(ctx).RetireWebhookSecret(ctx, secretID)
	if err != nil {
		return fmt.Errorf("postgres: retire webhook secret: %w", err)
	}
	if affected == 0 {
		return webhooks.ErrSecretNotFound
	}
	return nil
}

func (r *WebhookRepo) EnqueueDeliveries(ctx context.Context, deliveries []*webhooks.Delivery) error {
	q := r.q(ctx)

	for _, d := range deliveries {
		err := q.EnqueueDelivery(ctx, sqlc.EnqueueDeliveryParams{
			ID:         d.ID,
			EventID:    d.EventID,
			EndpointID: d.EndpointID,
			MerchantID: d.MerchantID,
		})
		if err != nil {
			return fmt.Errorf("postgres: enqueue delivery: %w", err)
		}
	}
	return nil
}

func (r *WebhookRepo) ClaimDue(ctx context.Context, limit int, leaseUntil time.Time) ([]*webhooks.Delivery, error) {
	rows, err := r.q(ctx).ClaimDueDeliveries(ctx, sqlc.ClaimDueDeliveriesParams{
		Limit:       int32(limit),
		LeasedUntil: timestamptz(leaseUntil),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: claim due deliveries: %w", err)
	}

	out := make([]*webhooks.Delivery, 0, len(rows))
	for _, row := range rows {
		out = append(out, deliveryFromRow(sqlc.WebhookDelivery(row)))
	}
	return out, nil
}

func (r *WebhookRepo) CompleteDelivery(ctx context.Context, deliveryID string, state webhooks.DeliveryState, resp webhooks.Response) error {
	affected, err := r.q(ctx).CompleteDelivery(ctx, sqlc.CompleteDeliveryParams{
		ID:             deliveryID,
		State:          sqlc.DeliveryState(state),
		LastStatus:     statusOrNil(resp),
		LastError:      errText(resp),
		LastDurationMs: durationMS(resp),
	})
	if err != nil {
		return fmt.Errorf("postgres: complete delivery: %w", err)
	}
	if affected == 0 {
		return webhooks.ErrDeliveryNotFound
	}
	return nil
}

func (r *WebhookRepo) RescheduleDelivery(ctx context.Context, deliveryID string, delay time.Duration, resp webhooks.Response) error {
	affected, err := r.q(ctx).RescheduleDelivery(ctx, sqlc.RescheduleDeliveryParams{
		ID:             deliveryID,
		DelaySeconds:   delay.Seconds(),
		LastStatus:     statusOrNil(resp),
		LastError:      errText(resp),
		LastDurationMs: durationMS(resp),
	})
	if err != nil {
		return fmt.Errorf("postgres: reschedule delivery: %w", err)
	}
	if affected == 0 {
		return webhooks.ErrDeliveryNotFound
	}
	return nil
}

func (r *WebhookRepo) RecordAttempt(ctx context.Context, a *webhooks.Attempt) error {
	err := r.q(ctx).RecordDeliveryAttempt(ctx, sqlc.RecordDeliveryAttemptParams{
		DeliveryID:  a.DeliveryID,
		Attempt:     int32(a.Attempt),
		StatusCode:  int32PtrFrom(a.StatusCode),
		Error:       nullString(a.Error),
		DurationMs:  int32PtrFrom(a.DurationMS),
		AttemptedAt: timestamptz(a.AttemptedAt),
	})
	if err != nil {
		return fmt.Errorf("postgres: record delivery attempt: %w", err)
	}
	return nil
}

func (r *WebhookRepo) ReclaimExpiredLeases(ctx context.Context, now time.Time) (int64, error) {
	n, err := r.q(ctx).ReclaimExpiredLeases(ctx, timestamptz(now))
	if err != nil {
		return 0, fmt.Errorf("postgres: reclaim expired leases: %w", err)
	}
	return n, nil
}

func (r *WebhookRepo) ListDeliveries(ctx context.Context, endpointID string, state webhooks.DeliveryState, limit int) ([]*webhooks.Delivery, error) {
	if limit <= 0 {
		limit = 100
	}

	var filter *sqlc.DeliveryState
	if state != "" {
		s := sqlc.DeliveryState(state)
		filter = &s
	}

	rows, err := r.q(ctx).ListDeliveries(ctx, sqlc.ListDeliveriesParams{
		EndpointID: endpointID,
		State:      filter,
		Limit:      int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("postgres: list deliveries: %w", err)
	}

	out := make([]*webhooks.Delivery, 0, len(rows))
	for _, row := range rows {
		out = append(out, deliveryFromRow(sqlc.WebhookDelivery(row)))
	}
	return out, nil
}

// ReplayDelivery moves an exhausted delivery back into the queue.
//
// The state check lives in the query, so replaying something already pending cannot
// reset its attempt counter and hand it a retry budget it has not earned.
func (r *WebhookRepo) ReplayDelivery(ctx context.Context, merchantID, deliveryID string) error {
	affected, err := r.q(ctx).ReplayDelivery(ctx, sqlc.ReplayDeliveryParams{
		ID:         deliveryID,
		MerchantID: merchantID,
	})
	if err != nil {
		return fmt.Errorf("postgres: replay delivery: %w", err)
	}
	if affected == 0 {
		return webhooks.ErrNotReplayable
	}
	return nil
}

func (r *WebhookRepo) GetEvent(ctx context.Context, eventID string) (*events.Event, error) {
	row, err := r.q(ctx).GetEvent(ctx, eventID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", events.ErrEventNotFound, eventID)
	}
	if err != nil {
		return nil, fmt.Errorf("postgres: get event: %w", err)
	}
	return eventFromRow(row), nil
}

// QueueDepth reports how many deliveries sit in each state, for the metrics gauge.
func (r *WebhookRepo) QueueDepth(ctx context.Context) (map[webhooks.DeliveryState]int64, error) {
	rows, err := r.q(ctx).CountDeliveriesByState(ctx)
	if err != nil {
		return nil, fmt.Errorf("postgres: queue depth: %w", err)
	}

	out := make(map[webhooks.DeliveryState]int64, len(rows))
	for _, row := range rows {
		out[webhooks.DeliveryState(row.State)] = row.Total
	}
	return out, nil
}

// OldestPendingSeconds is the real service-level indicator: not the queue's size, but
// how long the oldest thing in it has been waiting.
func (r *WebhookRepo) OldestPendingSeconds(ctx context.Context) (int64, error) {
	seconds, err := r.q(ctx).OldestPendingDelivery(ctx)
	if err != nil {
		return 0, fmt.Errorf("postgres: oldest pending delivery: %w", err)
	}
	return seconds, nil
}

func endpointFromRow(row sqlc.WebhookEndpoint) *webhooks.Endpoint {
	e := &webhooks.Endpoint{
		ID:                  row.ID,
		MerchantID:          row.MerchantID,
		URL:                 row.Url,
		EnabledEvents:       row.EnabledEvents,
		Status:              webhooks.EndpointStatus(row.Status),
		ConsecutiveFailures: int(row.ConsecutiveFailures),
		Description:         row.Description,
		CreatedAt:           row.CreatedAt.Time,
	}
	if row.DisabledAt.Valid {
		at := row.DisabledAt.Time
		e.DisabledAt = &at
	}
	return e
}

func deliveryFromRow(row sqlc.WebhookDelivery) *webhooks.Delivery {
	d := &webhooks.Delivery{
		ID:            row.ID,
		EventID:       row.EventID,
		EndpointID:    row.EndpointID,
		MerchantID:    row.MerchantID,
		State:         webhooks.DeliveryState(row.State),
		Attempt:       int(row.Attempt),
		NextAttemptAt: row.NextAttemptAt.Time,
		LastError:     derefString(row.LastError),
		CreatedAt:     row.CreatedAt.Time,
	}
	if row.LeasedUntil.Valid {
		at := row.LeasedUntil.Time
		d.LeasedUntil = &at
	}
	if row.CompletedAt.Valid {
		at := row.CompletedAt.Time
		d.CompletedAt = &at
	}
	if row.LastStatus != nil {
		n := int(*row.LastStatus)
		d.LastStatus = &n
	}
	if row.LastDurationMs != nil {
		n := int(*row.LastDurationMs)
		d.LastDurationMS = &n
	}
	return d
}

func statusOrNil(r webhooks.Response) *int32 {
	if r.StatusCode == 0 {
		return nil
	}
	n := int32(r.StatusCode)
	return &n
}

func errText(r webhooks.Response) *string {
	if r.Err == nil {
		return nil
	}
	s := r.Err.Error()
	return &s
}

func durationMS(r webhooks.Response) *int32 {
	n := int32(r.Duration.Milliseconds())
	return &n
}

func int32PtrFrom(v *int) *int32 {
	if v == nil {
		return nil
	}
	n := int32(*v)
	return &n
}
