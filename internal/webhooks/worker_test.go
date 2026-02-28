package webhooks

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/events"
	"github.com/Madhav-000-s/ledgerd/internal/platform/id"
	"github.com/Madhav-000-s/ledgerd/pkg/webhookverify"
)

// ── in-memory stores ────────────────────────────────────────────────────────

// memEvents is an in-memory outbox, enough to drive the dispatcher.
type memEvents struct {
	mu     sync.Mutex
	events []*events.Event
}

func (s *memEvents) Append(_ context.Context, e *events.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
	return nil
}

func (s *memEvents) Get(_ context.Context, _, eventID string) (*events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.ID == eventID {
			return e, nil
		}
	}
	return nil, events.ErrEventNotFound
}

func (s *memEvents) List(context.Context, string, int, string) ([]*events.Event, string, error) {
	return nil, "", nil
}

func (s *memEvents) ClaimUndispatched(_ context.Context, limit int) ([]*events.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*events.Event, 0, limit)
	for _, e := range s.events {
		if e.DispatchedAt == nil && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *memEvents) MarkDispatched(_ context.Context, ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	for _, wanted := range ids {
		for _, e := range s.events {
			if e.ID == wanted {
				e.DispatchedAt = &now
			}
		}
	}
	return nil
}

// memWebhooks is an in-memory webhooks.Store.
//
// It emulates the queue's bookkeeping but not its locking: SKIP LOCKED is a property of
// Postgres and is asserted against real Postgres in the integration suite. A fake that
// pretended to lock would let a concurrency bug pass here.
type memWebhooks struct {
	mu         sync.Mutex
	endpoints  map[string]*Endpoint
	secrets    map[string][]*Secret
	deliveries map[string]*Delivery
	attempts   []*Attempt
	eventStore *memEvents
	failCreate error
}

func newMemWebhooks(ev *memEvents) *memWebhooks {
	return &memWebhooks{
		endpoints:  map[string]*Endpoint{},
		secrets:    map[string][]*Secret{},
		deliveries: map[string]*Delivery{},
		eventStore: ev,
	}
}

func (s *memWebhooks) CreateEndpoint(_ context.Context, e *Endpoint) error {
	if s.failCreate != nil {
		return s.failCreate
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := *e
	s.endpoints[e.ID] = &clone
	return nil
}

func (s *memWebhooks) GetEndpoint(_ context.Context, _, endpointID string) (*Endpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.endpoints[endpointID]
	if !ok {
		return nil, ErrEndpointNotFound
	}
	clone := *e
	return &clone, nil
}

func (s *memWebhooks) ListEndpoints(_ context.Context, merchantID string) ([]*Endpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*Endpoint, 0, len(s.endpoints))
	for _, e := range s.endpoints {
		if e.MerchantID == merchantID {
			clone := *e
			out = append(out, &clone)
		}
	}
	return out, nil
}

func (s *memWebhooks) ActiveEndpointsForEvent(ctx context.Context, merchantID string) ([]*Endpoint, error) {
	all, err := s.ListEndpoints(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	out := make([]*Endpoint, 0, len(all))
	for _, e := range all {
		if e.Status != EndpointDisabled {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *memWebhooks) SetEndpointStatus(_ context.Context, endpointID string, status EndpointStatus) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.endpoints[endpointID]
	if !ok {
		return ErrEndpointNotFound
	}
	e.Status = status
	return nil
}

func (s *memWebhooks) RecordEndpointFailure(_ context.Context, endpointID string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.endpoints[endpointID]
	if !ok {
		return 0, ErrEndpointNotFound
	}
	e.ConsecutiveFailures++
	return e.ConsecutiveFailures, nil
}

func (s *memWebhooks) ResetEndpointFailures(_ context.Context, endpointID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.endpoints[endpointID]; ok {
		e.ConsecutiveFailures = 0
	}
	return nil
}

func (s *memWebhooks) CreateSecret(_ context.Context, sec *Secret, _, _ []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := *sec
	s.secrets[sec.EndpointID] = append(s.secrets[sec.EndpointID], &clone)
	return nil
}

func (s *memWebhooks) ActiveSecrets(_ context.Context, endpointID string) ([]*Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*Secret, 0, 2)
	for _, sec := range s.secrets[endpointID] {
		if sec.Active {
			clone := *sec
			out = append(out, &clone)
		}
	}
	if len(out) == 0 {
		return nil, ErrSecretNotFound
	}
	return out, nil
}

func (s *memWebhooks) RetireSecret(_ context.Context, secretID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, list := range s.secrets {
		for _, sec := range list {
			if sec.ID == secretID {
				sec.Active = false
				return nil
			}
		}
	}
	return ErrSecretNotFound
}

func (s *memWebhooks) EnqueueDeliveries(_ context.Context, ds []*Delivery) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, d := range ds {
		// The real store's UNIQUE (event_id, endpoint_id) makes fan-out idempotent.
		duplicate := false
		for _, existing := range s.deliveries {
			if existing.EventID == d.EventID && existing.EndpointID == d.EndpointID {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		clone := *d
		clone.State = StatePending
		s.deliveries[d.ID] = &clone
	}
	return nil
}

func (s *memWebhooks) ClaimDue(_ context.Context, limit int, leaseUntil time.Time) ([]*Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now().UTC()
	out := make([]*Delivery, 0, limit)
	for _, d := range s.deliveries {
		if len(out) >= limit {
			break
		}
		if d.State != StatePending || d.NextAttemptAt.After(now) {
			continue
		}
		d.State = StateDelivering
		at := leaseUntil
		d.LeasedUntil = &at

		clone := *d
		out = append(out, &clone)
	}
	return out, nil
}

func (s *memWebhooks) CompleteDelivery(_ context.Context, deliveryID string, state DeliveryState, resp Response) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.deliveries[deliveryID]
	if !ok {
		return ErrDeliveryNotFound
	}
	d.State = state
	d.Attempt++
	d.LeasedUntil = nil
	now := time.Now().UTC()
	d.CompletedAt = &now
	if resp.StatusCode != 0 {
		code := resp.StatusCode
		d.LastStatus = &code
	}
	return nil
}

func (s *memWebhooks) RescheduleDelivery(_ context.Context, deliveryID string, delay time.Duration, resp Response) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.deliveries[deliveryID]
	if !ok {
		return ErrDeliveryNotFound
	}
	d.State = StatePending
	d.Attempt++
	d.NextAttemptAt = time.Now().UTC().Add(delay)
	d.LeasedUntil = nil
	if resp.StatusCode != 0 {
		code := resp.StatusCode
		d.LastStatus = &code
	}
	return nil
}

func (s *memWebhooks) RecordAttempt(_ context.Context, a *Attempt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := *a
	s.attempts = append(s.attempts, &clone)
	return nil
}

func (s *memWebhooks) ReclaimExpiredLeases(_ context.Context, now time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var n int64
	for _, d := range s.deliveries {
		if d.State == StateDelivering && d.LeasedUntil != nil && d.LeasedUntil.Before(now) {
			d.State = StatePending
			d.LeasedUntil = nil
			n++
		}
	}
	return n, nil
}

func (s *memWebhooks) ListDeliveries(_ context.Context, endpointID string, state DeliveryState, limit int) ([]*Delivery, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*Delivery, 0, limit)
	for _, d := range s.deliveries {
		if d.EndpointID != endpointID {
			continue
		}
		if state != "" && d.State != state {
			continue
		}
		clone := *d
		out = append(out, &clone)
	}
	return out, nil
}

func (s *memWebhooks) ReplayDelivery(_ context.Context, _, deliveryID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	d, ok := s.deliveries[deliveryID]
	if !ok || d.State != StateExhausted {
		return ErrNotReplayable
	}
	d.State = StatePending
	d.Attempt = 0
	d.NextAttemptAt = time.Now().UTC()
	d.CompletedAt = nil
	return nil
}

func (s *memWebhooks) GetEvent(ctx context.Context, eventID string) (*events.Event, error) {
	return s.eventStore.Get(ctx, "", eventID)
}

// passthroughTx runs the closure directly. Atomicity is a database property and is
// asserted against real Postgres.
type passthroughTx struct{}

func (passthroughTx) InTx(ctx context.Context, fn func(context.Context) error) error {
	return fn(ctx)
}

// ── harness ─────────────────────────────────────────────────────────────────

const testMerchant = "mer_test"

type harness struct {
	t          *testing.T
	ctx        context.Context
	eventStore *memEvents
	store      *memWebhooks
	dispatcher *Dispatcher
	cipher     *Cipher
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	cipher, err := NewCipher(strings.Repeat("ef", 32))
	if err != nil {
		t.Fatalf("NewCipher = %v", err)
	}

	ev := &memEvents{}
	store := newMemWebhooks(ev)

	return &harness{
		t:          t,
		ctx:        context.Background(),
		eventStore: ev,
		store:      store,
		dispatcher: NewDispatcher(ev, store, passthroughTx{}, 100, nil),
		cipher:     cipher,
	}
}

func (h *harness) endpoint(url string, filters ...string) (*Endpoint, string) {
	h.t.Helper()

	if len(filters) == 0 {
		filters = []string{"*"}
	}

	ep := &Endpoint{
		ID:            id.New(id.PrefixEndpoint),
		MerchantID:    testMerchant,
		URL:           url,
		EnabledEvents: filters,
		Status:        EndpointActive,
		CreatedAt:     time.Now().UTC(),
	}
	if err := h.store.CreateEndpoint(h.ctx, ep); err != nil {
		h.t.Fatalf("CreateEndpoint = %v", err)
	}

	plaintext, secret, hash, encrypted, err := GenerateSecret(ep.ID, h.cipher)
	if err != nil {
		h.t.Fatalf("GenerateSecret = %v", err)
	}
	if err := h.store.CreateSecret(h.ctx, secret, hash, encrypted); err != nil {
		h.t.Fatalf("CreateSecret = %v", err)
	}
	return ep, plaintext
}

func (h *harness) emit(typ events.Type) *events.Event {
	h.t.Helper()

	e := &events.Event{
		ID:         id.New(id.PrefixEvent),
		MerchantID: testMerchant,
		Type:       typ,
		ResourceID: "res_1",
		Payload:    json.RawMessage(`{"amount":10000}`),
		CreatedAt:  time.Now().UTC(),
	}
	if err := h.eventStore.Append(h.ctx, e); err != nil {
		h.t.Fatalf("Append = %v", err)
	}
	return e
}

func (h *harness) worker(maxAttempts int) *Worker {
	return NewWorker(WorkerOptions{
		Store: h.store,
		Client: NewClient(ClientOptions{
			AllowInsecure:   true,
			AllowPrivateIPs: true,
			RequestTimeout:  2 * time.Second,
		}),
		Cipher:      h.cipher,
		Backoff:     Backoff{Base: time.Millisecond, Cap: 2 * time.Millisecond},
		Tx:          passthroughTx{},
		MaxAttempts: maxAttempts,
		Lease:       time.Minute,
		BatchSize:   50,
	})
}

func (h *harness) deliveries(endpointID string) []*Delivery {
	h.t.Helper()

	out, err := h.store.ListDeliveries(h.ctx, endpointID, "", 100)
	if err != nil {
		h.t.Fatalf("ListDeliveries = %v", err)
	}
	return out
}

// ── dispatcher ──────────────────────────────────────────────────────────────

func TestDispatcherFansOutToSubscribedEndpoints(t *testing.T) {
	h := newHarness(t)

	all, _ := h.endpoint("https://a.example.com/hook", "*")
	paymentsOnly, _ := h.endpoint("https://b.example.com/hook", "payment.*")
	transfersOnly, _ := h.endpoint("https://c.example.com/hook", "transfer.posted")

	h.emit(events.TypePaymentSucceeded)

	n, err := h.dispatcher.DispatchOnce(h.ctx)
	if err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}
	if n != 1 {
		t.Fatalf("dispatched %d events, want 1", n)
	}

	if got := len(h.deliveries(all.ID)); got != 1 {
		t.Errorf("the wildcard endpoint got %d deliveries, want 1", got)
	}
	if got := len(h.deliveries(paymentsOnly.ID)); got != 1 {
		t.Errorf("the payment.* endpoint got %d deliveries, want 1", got)
	}
	if got := len(h.deliveries(transfersOnly.ID)); got != 0 {
		t.Errorf("the transfer.posted endpoint got %d deliveries for a payment event, want 0", got)
	}
}

// An event with no matching endpoint is still marked dispatched, or the dispatcher would
// re-examine it on every pass forever.
func TestDispatcherMarksUnmatchedEventsDispatched(t *testing.T) {
	h := newHarness(t)
	h.endpoint("https://a.example.com/hook", "transfer.*")

	h.emit(events.TypePaymentSucceeded)

	if _, err := h.dispatcher.DispatchOnce(h.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}

	// A second pass finds nothing left to do.
	n, err := h.dispatcher.DispatchOnce(h.ctx)
	if err != nil {
		t.Fatalf("second DispatchOnce = %v", err)
	}
	if n != 0 {
		t.Errorf("a second pass dispatched %d events; the first left one unmarked", n)
	}
}

func TestDispatcherSkipsDisabledEndpoints(t *testing.T) {
	h := newHarness(t)
	ep, _ := h.endpoint("https://a.example.com/hook")

	if err := h.store.SetEndpointStatus(h.ctx, ep.ID, EndpointDisabled); err != nil {
		t.Fatalf("SetEndpointStatus = %v", err)
	}

	h.emit(events.TypePaymentSucceeded)
	if _, err := h.dispatcher.DispatchOnce(h.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}

	if got := len(h.deliveries(ep.ID)); got != 0 {
		t.Errorf("a disabled endpoint got %d deliveries, want 0", got)
	}
}

func TestDispatcherIsIdempotent(t *testing.T) {
	h := newHarness(t)
	ep, _ := h.endpoint("https://a.example.com/hook")
	e := h.emit(events.TypePaymentSucceeded)

	if _, err := h.dispatcher.DispatchOnce(h.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}

	// Re-run the expansion by hand, as a crashed dispatcher's retry would.
	if err := h.store.EnqueueDeliveries(h.ctx, []*Delivery{{
		ID:         id.New(id.PrefixDelivery),
		EventID:    e.ID,
		EndpointID: ep.ID,
		MerchantID: testMerchant,
	}}); err != nil {
		t.Fatalf("re-enqueue = %v", err)
	}

	if got := len(h.deliveries(ep.ID)); got != 1 {
		t.Errorf("%d deliveries after a repeated fan-out, want 1", got)
	}
}

func TestDispatcherHandlesAnEmptyOutbox(t *testing.T) {
	h := newHarness(t)

	n, err := h.dispatcher.DispatchOnce(h.ctx)
	if err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}
	if n != 0 {
		t.Errorf("dispatched %d events from an empty outbox", n)
	}
}

// ── the delivery worker ─────────────────────────────────────────────────────

func TestWorkerDeliversAndSigns(t *testing.T) {
	h := newHarness(t)

	var (
		mu        sync.Mutex
		gotBody   []byte
		gotHeader string
		gotTopic  string
		gotID     string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		gotBody, _ = io.ReadAll(r.Body)
		gotHeader = r.Header.Get(webhookverify.Header)
		gotTopic = r.Header.Get("Webhook-Topic")
		gotID = r.Header.Get("Webhook-Id")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ep, secret := h.endpoint(server.URL)
	event := h.emit(events.TypePaymentSucceeded)

	if _, err := h.dispatcher.DispatchOnce(h.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}
	n, err := h.worker(16).DeliverOnce(h.ctx)
	if err != nil {
		t.Fatalf("DeliverOnce = %v", err)
	}
	if n != 1 {
		t.Fatalf("delivered %d, want 1", n)
	}

	mu.Lock()
	defer mu.Unlock()

	if err := webhookverify.Verify(gotHeader, gotBody, secret, 0); err != nil {
		t.Errorf("the delivered payload did not verify: %v", err)
	}
	if gotTopic != string(events.TypePaymentSucceeded) {
		t.Errorf("Webhook-Topic = %q", gotTopic)
	}
	if gotID != event.ID {
		t.Errorf("Webhook-Id = %q, want %q", gotID, event.ID)
	}

	d := h.deliveries(ep.ID)[0]
	if d.State != StateSucceeded {
		t.Errorf("state = %s, want succeeded", d.State)
	}
	if len(h.store.attempts) != 1 {
		t.Errorf("%d attempts recorded, want 1", len(h.store.attempts))
	}
}

// A success clears the failure streak, so an endpoint that recovers is not disabled by
// history it has already made up for.
func TestSuccessResetsTheFailureStreak(t *testing.T) {
	h := newHarness(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ep, _ := h.endpoint(server.URL)
	if _, err := h.store.RecordEndpointFailure(h.ctx, ep.ID); err != nil {
		t.Fatalf("RecordEndpointFailure = %v", err)
	}

	h.emit(events.TypePaymentSucceeded)
	if _, err := h.dispatcher.DispatchOnce(h.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}
	if _, err := h.worker(16).DeliverOnce(h.ctx); err != nil {
		t.Fatalf("DeliverOnce = %v", err)
	}

	after, err := h.store.GetEndpoint(h.ctx, testMerchant, ep.ID)
	if err != nil {
		t.Fatalf("GetEndpoint = %v", err)
	}
	if after.ConsecutiveFailures != 0 {
		t.Errorf("failures = %d after a success, want 0", after.ConsecutiveFailures)
	}
}

func TestWorkerRetriesUntilExhausted(t *testing.T) {
	h := newHarness(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	ep, _ := h.endpoint(server.URL)
	h.emit(events.TypePaymentSucceeded)

	if _, err := h.dispatcher.DispatchOnce(h.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}

	worker := h.worker(3)
	for i := 0; i < 6; i++ {
		if _, err := worker.DeliverOnce(h.ctx); err != nil {
			t.Fatalf("DeliverOnce = %v", err)
		}
		if h.deliveries(ep.ID)[0].State == StateExhausted {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	d := h.deliveries(ep.ID)[0]
	if d.State != StateExhausted {
		t.Fatalf("state = %s, want exhausted", d.State)
	}
	if d.Attempt != 3 {
		t.Errorf("attempts = %d, want the budget of 3", d.Attempt)
	}

	after, _ := h.store.GetEndpoint(h.ctx, testMerchant, ep.ID)
	if after.ConsecutiveFailures != 1 {
		t.Errorf("endpoint failures = %d, want 1", after.ConsecutiveFailures)
	}
}

// 410 Gone is the one status whose meaning is "stop sending".
func TestWorkerDisablesOnGone(t *testing.T) {
	h := newHarness(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer server.Close()

	ep, _ := h.endpoint(server.URL)
	h.emit(events.TypePaymentSucceeded)

	if _, err := h.dispatcher.DispatchOnce(h.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}
	if _, err := h.worker(16).DeliverOnce(h.ctx); err != nil {
		t.Fatalf("DeliverOnce = %v", err)
	}

	after, _ := h.store.GetEndpoint(h.ctx, testMerchant, ep.ID)
	if after.Status != EndpointDisabled {
		t.Errorf("status = %s, want disabled", after.Status)
	}
	if got := h.deliveries(ep.ID)[0].State; got != StateDisabled {
		t.Errorf("delivery state = %s, want disabled", got)
	}
}

// Crossing the degrade threshold marks the endpoint degraded and raises an alert; the
// disable threshold stops delivery entirely.
func TestEndpointHealthThresholds(t *testing.T) {
	h := newHarness(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	ep, _ := h.endpoint(server.URL)

	// Walk the counter up to just below degraded.
	for i := 0; i < DegradeAfter-1; i++ {
		if _, err := h.store.RecordEndpointFailure(h.ctx, ep.ID); err != nil {
			t.Fatalf("RecordEndpointFailure = %v", err)
		}
	}
	if got, _ := h.store.GetEndpoint(h.ctx, testMerchant, ep.ID); got.Status != EndpointActive {
		t.Fatalf("status = %s below the threshold, want active", got.Status)
	}

	// One exhausted delivery tips it over.
	h.emit(events.TypePaymentSucceeded)
	if _, err := h.dispatcher.DispatchOnce(h.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}
	worker := h.worker(1) // exhaust on the first attempt
	if _, err := worker.DeliverOnce(h.ctx); err != nil {
		t.Fatalf("DeliverOnce = %v", err)
	}

	after, _ := h.store.GetEndpoint(h.ctx, testMerchant, ep.ID)
	if after.Status != EndpointDegraded {
		t.Errorf("status = %s at %d failures, want degraded", after.Status, after.ConsecutiveFailures)
	}
}

// A delivery for an endpoint that was disabled after the row was queued must not be sent.
func TestWorkerSkipsDisabledEndpointAtDeliveryTime(t *testing.T) {
	h := newHarness(t)

	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ep, _ := h.endpoint(server.URL)
	h.emit(events.TypePaymentSucceeded)

	if _, err := h.dispatcher.DispatchOnce(h.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}
	// Disabled between fan-out and delivery.
	if err := h.store.SetEndpointStatus(h.ctx, ep.ID, EndpointDisabled); err != nil {
		t.Fatalf("SetEndpointStatus = %v", err)
	}

	if _, err := h.worker(16).DeliverOnce(h.ctx); err != nil {
		t.Fatalf("DeliverOnce = %v", err)
	}

	if hits != 0 {
		t.Errorf("the endpoint was called %d times after being disabled, want 0", hits)
	}
	if got := h.deliveries(ep.ID)[0].State; got != StateDisabled {
		t.Errorf("delivery state = %s, want disabled", got)
	}
}

// A transport failure is retried, and the attempt is recorded with no status code.
func TestWorkerRetriesTransportFailures(t *testing.T) {
	h := newHarness(t)

	// A server that is closed immediately, so the connection is refused.
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := server.URL
	server.Close()

	ep, _ := h.endpoint(url)
	h.emit(events.TypePaymentSucceeded)

	if _, err := h.dispatcher.DispatchOnce(h.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}
	if _, err := h.worker(16).DeliverOnce(h.ctx); err != nil {
		t.Fatalf("DeliverOnce = %v", err)
	}

	d := h.deliveries(ep.ID)[0]
	if d.State != StatePending {
		t.Errorf("state = %s, want pending for a retry", d.State)
	}
	if len(h.store.attempts) != 1 {
		t.Fatalf("%d attempts recorded, want 1", len(h.store.attempts))
	}
	if h.store.attempts[0].Error == "" {
		t.Error("a transport failure recorded no error text")
	}
	if h.store.attempts[0].StatusCode != nil {
		t.Errorf("a transport failure recorded status %d", *h.store.attempts[0].StatusCode)
	}
}

// An endpoint with no signing secret cannot be delivered to, and that must surface as an
// error rather than an unsigned request.
func TestWorkerRefusesToSendUnsigned(t *testing.T) {
	h := newHarness(t)

	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	// An endpoint created without a secret.
	ep := &Endpoint{
		ID: id.New(id.PrefixEndpoint), MerchantID: testMerchant, URL: server.URL,
		EnabledEvents: []string{"*"}, Status: EndpointActive, CreatedAt: time.Now().UTC(),
	}
	if err := h.store.CreateEndpoint(h.ctx, ep); err != nil {
		t.Fatalf("CreateEndpoint = %v", err)
	}

	h.emit(events.TypePaymentSucceeded)
	if _, err := h.dispatcher.DispatchOnce(h.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}
	// DeliverOnce logs the per-delivery error and continues, so it returns no error here.
	if _, err := h.worker(16).DeliverOnce(h.ctx); err != nil {
		t.Fatalf("DeliverOnce = %v", err)
	}

	if hits != 0 {
		t.Errorf("an unsigned request was sent %d times, want 0", hits)
	}
}

func TestReclaimOnce(t *testing.T) {
	h := newHarness(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ep, _ := h.endpoint(server.URL)
	h.emit(events.TypePaymentSucceeded)
	if _, err := h.dispatcher.DispatchOnce(h.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}

	// Claim without delivering, then expire the lease.
	claimed, err := h.store.ClaimDue(h.ctx, 10, time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatalf("ClaimDue = %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d, want 1", len(claimed))
	}

	n, err := h.worker(16).ReclaimOnce(h.ctx)
	if err != nil {
		t.Fatalf("ReclaimOnce = %v", err)
	}
	if n != 1 {
		t.Errorf("reclaimed %d, want 1", n)
	}
	if got := h.deliveries(ep.ID)[0].State; got != StatePending {
		t.Errorf("state = %s after reclaim, want pending", got)
	}
}

func TestReplayOnlyFromExhausted(t *testing.T) {
	h := newHarness(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ep, _ := h.endpoint(server.URL)
	h.emit(events.TypePaymentSucceeded)
	if _, err := h.dispatcher.DispatchOnce(h.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}
	if _, err := h.worker(16).DeliverOnce(h.ctx); err != nil {
		t.Fatalf("DeliverOnce = %v", err)
	}

	succeeded := h.deliveries(ep.ID)[0]
	if err := h.store.ReplayDelivery(h.ctx, testMerchant, succeeded.ID); !errors.Is(err, ErrNotReplayable) {
		t.Errorf("replaying a succeeded delivery = %v, want ErrNotReplayable", err)
	}
}

func TestSecretRotationRetiresTheOldOne(t *testing.T) {
	h := newHarness(t)
	ep, first := h.endpoint("https://a.example.com/hook")

	// Add a second secret, as a rotation would.
	second, secret, hash, encrypted, err := GenerateSecret(ep.ID, h.cipher)
	if err != nil {
		t.Fatalf("GenerateSecret = %v", err)
	}
	if err := h.store.CreateSecret(h.ctx, secret, hash, encrypted); err != nil {
		t.Fatalf("CreateSecret = %v", err)
	}

	active, err := h.store.ActiveSecrets(h.ctx, ep.ID)
	if err != nil {
		t.Fatalf("ActiveSecrets = %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("%d active secrets during the overlap, want 2", len(active))
	}

	// Both sign, which is what makes the overlap useful.
	body := []byte(`{"id":"evt_1"}`)
	now := time.Now().UTC()
	header, _, err := NewSigner(func() time.Time { return now }).Sign(body, active)
	if err != nil {
		t.Fatalf("Sign = %v", err)
	}
	for name, s := range map[string]string{"first": first, "second": second} {
		if err := webhookverify.VerifyAt(header, body, s, 0, now); err != nil {
			t.Errorf("the %s secret did not verify during the overlap: %v", name, err)
		}
	}

	// Retiring the first leaves only the second.
	if err := h.store.RetireSecret(h.ctx, active[len(active)-1].ID); err != nil {
		t.Fatalf("RetireSecret = %v", err)
	}
	remaining, err := h.store.ActiveSecrets(h.ctx, ep.ID)
	if err != nil {
		t.Fatalf("ActiveSecrets = %v", err)
	}
	if len(remaining) != 1 {
		t.Errorf("%d active secrets after retiring one, want 1", len(remaining))
	}
}
