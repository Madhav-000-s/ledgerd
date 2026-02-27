//go:build integration

package integration

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Madhav-000-s/ledgerd/internal/events"
	"github.com/Madhav-000-s/ledgerd/internal/ledger"
	"github.com/Madhav-000-s/ledgerd/internal/money"
	"github.com/Madhav-000-s/ledgerd/internal/platform/config"
	"github.com/Madhav-000-s/ledgerd/internal/platform/id"
	"github.com/Madhav-000-s/ledgerd/internal/postgres"
	"github.com/Madhav-000-s/ledgerd/internal/webhooks"
	"github.com/Madhav-000-s/ledgerd/pkg/webhookverify"
)

// webhookEnv is the outbox, the queue and a worker wired to a test endpoint.
type webhookEnv struct {
	*env
	eventRepo   *postgres.EventRepo
	webhookRepo *postgres.WebhookRepo
	dispatcher  *webhooks.Dispatcher
	cipher      *webhooks.Cipher
	recorder    *events.Recorder
}

func newWebhookEnv(t *testing.T) *webhookEnv {
	t.Helper()

	e := newEnv(t, config.IsolationReadCommitted)

	cipher, err := webhooks.NewCipher(strings.Repeat("cd", 32))
	if err != nil {
		t.Fatalf("NewCipher = %v", err)
	}

	eventRepo := postgres.NewEventRepo(e.db)
	webhookRepo := postgres.NewWebhookRepo(e.db, cipher)

	return &webhookEnv{
		env:         e,
		eventRepo:   eventRepo,
		webhookRepo: webhookRepo,
		cipher:      cipher,
		recorder:    events.NewRecorder(eventRepo, nil),
		dispatcher:  webhooks.NewDispatcher(eventRepo, webhookRepo, e.txm, 100, nil),
	}
}

// endpoint registers a webhook endpoint pointing at url and returns it with its secret.
func (w *webhookEnv) endpoint(t *testing.T, url string, filters ...string) (*webhooks.Endpoint, string) {
	t.Helper()

	if len(filters) == 0 {
		filters = []string{"*"}
	}

	ep := &webhooks.Endpoint{
		ID:            id.New(id.PrefixEndpoint),
		MerchantID:    testMerchant,
		URL:           url,
		EnabledEvents: filters,
		Status:        webhooks.EndpointActive,
		CreatedAt:     time.Now().UTC(),
	}
	if err := w.webhookRepo.CreateEndpoint(w.ctx, ep); err != nil {
		t.Fatalf("CreateEndpoint = %v", err)
	}

	plaintext, secret, hash, encrypted, err := webhooks.GenerateSecret(ep.ID, w.cipher)
	if err != nil {
		t.Fatalf("GenerateSecret = %v", err)
	}
	if err := w.webhookRepo.CreateSecret(w.ctx, secret, hash, encrypted); err != nil {
		t.Fatalf("CreateSecret = %v", err)
	}
	return ep, plaintext
}

// worker builds a delivery worker with a fast backoff so retries are testable.
func (w *webhookEnv) worker(maxAttempts int) *webhooks.Worker {
	return webhooks.NewWorker(webhooks.WorkerOptions{
		Store: w.webhookRepo,
		Client: webhooks.NewClient(webhooks.ClientOptions{
			AllowInsecure:   true,
			AllowPrivateIPs: true,
			RequestTimeout:  2 * time.Second,
		}),
		Cipher:      w.cipher,
		Backoff:     webhooks.Backoff{Base: time.Millisecond, Cap: 5 * time.Millisecond},
		Tx:          w.txm,
		MaxAttempts: maxAttempts,
		Lease:       30 * time.Second,
		BatchSize:   50,
	})
}

// The outbox guarantee: the event and the ledger entries commit together or neither
// does. This is what removes the dual-write problem rather than working around it.
func TestOutboxCommitsWithTheLedgerWrite(t *testing.T) {
	w := newWebhookEnv(t)

	cash := w.account("cash", ledger.Asset, money.USD, true)
	payable := w.account("payable", ledger.Liability, money.USD, true)

	sentinel := errors.New("rolled back after both writes")

	err := w.inTx(func(ctx context.Context) error {
		txn, err := w.svc.Post(ctx, ledger.TransactionSpec{
			MerchantID: testMerchant,
			Entries: []ledger.EntrySpec{
				entry(cash.ID, ledger.Debit, 1000),
				entry(payable.ID, ledger.Credit, 1000),
			},
		})
		if err != nil {
			return err
		}
		if _, err := w.recorder.Record(ctx, testMerchant, events.TypeTransferPosted, txn.ID, map[string]any{
			"transaction_id": txn.ID,
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("InTx = %v, want the sentinel", err)
	}

	// Neither the money nor the event survived.
	if got := w.balance(cash.ID).Posted; got != 0 {
		t.Errorf("balance = %d after rollback, want 0", got)
	}
	var eventCount int64
	if err := w.db.Pool().QueryRow(w.ctx, `SELECT count(*) FROM events`).Scan(&eventCount); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if eventCount != 0 {
		t.Errorf("%d events survived a rolled-back transaction, want 0", eventCount)
	}

	// And on success, both are there.
	if err := w.inTx(func(ctx context.Context) error {
		txn, err := w.svc.Post(ctx, ledger.TransactionSpec{
			MerchantID: testMerchant,
			Entries: []ledger.EntrySpec{
				entry(cash.ID, ledger.Debit, 1000),
				entry(payable.ID, ledger.Credit, 1000),
			},
		})
		if err != nil {
			return err
		}
		_, err = w.recorder.Record(ctx, testMerchant, events.TypeTransferPosted, txn.ID, map[string]any{
			"transaction_id": txn.ID,
		})
		return err
	}); err != nil {
		t.Fatalf("InTx = %v", err)
	}

	if err := w.db.Pool().QueryRow(w.ctx, `SELECT count(*) FROM events`).Scan(&eventCount); err != nil {
		t.Fatalf("counting events: %v", err)
	}
	if eventCount != 1 {
		t.Errorf("%d events after a committed transaction, want 1", eventCount)
	}
}

// A delivery that succeeds first time, with a signature the reference verifier accepts.
func TestDeliverySucceedsAndVerifies(t *testing.T) {
	w := newWebhookEnv(t)

	var (
		mu        sync.Mutex
		gotBody   []byte
		gotHeader string
		hits      int
	)

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hits++
		gotBody = readBody(r)
		gotHeader = r.Header.Get(webhookverify.Header)
		rw.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	_, secret := w.endpoint(t, server.URL)
	w.emit(t, events.TypePaymentSucceeded, "pay_1")

	if n, err := w.dispatcher.DispatchOnce(w.ctx); err != nil || n != 1 {
		t.Fatalf("DispatchOnce = %d, %v; want 1, nil", n, err)
	}
	if n, err := w.worker(16).DeliverOnce(w.ctx); err != nil || n != 1 {
		t.Fatalf("DeliverOnce = %d, %v; want 1, nil", n, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("endpoint was called %d times, want 1", hits)
	}
	if err := webhookverify.Verify(gotHeader, gotBody, secret, 0); err != nil {
		t.Errorf("the delivered payload did not verify: %v", err)
	}
	if !strings.Contains(string(gotBody), "payment.succeeded") {
		t.Errorf("payload = %s", gotBody)
	}

	// The delivery is terminal and the endpoint's failure streak is clear.
	deliveries := w.deliveries(t, "")
	if len(deliveries) != 1 || deliveries[0].State != webhooks.StateSucceeded {
		t.Errorf("delivery state = %+v, want one succeeded", deliveries)
	}
}

// Fail N times then succeed. The retry budget is spent gradually rather than all at
// once, and the delivery ends succeeded rather than exhausted.
func TestDeliveryRetriesThenSucceeds(t *testing.T) {
	w := newWebhookEnv(t)

	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			rw.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		rw.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	w.endpoint(t, server.URL)
	w.emit(t, events.TypePaymentSucceeded, "pay_1")

	if _, err := w.dispatcher.DispatchOnce(w.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}

	worker := w.worker(16)
	for i := 0; i < 5; i++ {
		if _, err := worker.DeliverOnce(w.ctx); err != nil {
			t.Fatalf("DeliverOnce = %v", err)
		}
		if w.deliveries(t, "")[0].State == webhooks.StateSucceeded {
			break
		}
		// The backoff is milliseconds in this harness, so a short wait is enough for the
		// next attempt to become due.
		time.Sleep(20 * time.Millisecond)
	}

	if got := attempts.Load(); got != 3 {
		t.Errorf("endpoint saw %d attempts, want 3", got)
	}

	d := w.deliveries(t, "")[0]
	if d.State != webhooks.StateSucceeded {
		t.Errorf("state = %s, want succeeded", d.State)
	}
	if d.Attempt != 3 {
		t.Errorf("attempt counter = %d, want 3", d.Attempt)
	}

	// Every try is in the audit trail, not just the last one.
	var recorded int64
	if err := w.db.Pool().QueryRow(w.ctx, `SELECT count(*) FROM webhook_attempts`).Scan(&recorded); err != nil {
		t.Fatalf("counting attempts: %v", err)
	}
	if recorded != 3 {
		t.Errorf("%d attempts recorded, want 3", recorded)
	}
}

// An endpoint that never recovers exhausts its budget, lands in the DLQ, and can then be
// replayed by an operator.
func TestExhaustionLandsInTheDLQAndReplays(t *testing.T) {
	w := newWebhookEnv(t)

	var succeed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		if succeed.Load() {
			rw.WriteHeader(http.StatusOK)
			return
		}
		rw.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	endpoint, _ := w.endpoint(t, server.URL)
	w.emit(t, events.TypePaymentSucceeded, "pay_1")

	if _, err := w.dispatcher.DispatchOnce(w.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}

	// A compressed budget, so exhaustion is reachable in a test.
	worker := w.worker(3)
	for i := 0; i < 6; i++ {
		if _, err := worker.DeliverOnce(w.ctx); err != nil {
			t.Fatalf("DeliverOnce = %v", err)
		}
		if w.deliveries(t, "")[0].State == webhooks.StateExhausted {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	exhausted := w.deliveries(t, webhooks.StateExhausted)
	if len(exhausted) != 1 {
		t.Fatalf("%d deliveries in the DLQ, want 1", len(exhausted))
	}

	// The endpoint's consecutive-failure counter moved, which is what eventually
	// degrades and disables it.
	after, err := w.webhookRepo.GetEndpoint(w.ctx, testMerchant, endpoint.ID)
	if err != nil {
		t.Fatalf("GetEndpoint = %v", err)
	}
	if after.ConsecutiveFailures == 0 {
		t.Error("the endpoint's failure counter did not move after an exhausted delivery")
	}

	// An operator replays it once the endpoint is fixed.
	succeed.Store(true)
	if err := w.webhookRepo.ReplayDelivery(w.ctx, testMerchant, exhausted[0].ID); err != nil {
		t.Fatalf("ReplayDelivery = %v", err)
	}
	if _, err := worker.DeliverOnce(w.ctx); err != nil {
		t.Fatalf("DeliverOnce after replay = %v", err)
	}

	if got := w.deliveries(t, "")[0].State; got != webhooks.StateSucceeded {
		t.Errorf("state after replay = %s, want succeeded", got)
	}
}

// Replaying something that is not exhausted must be refused, or a caller could reset a
// live delivery's attempt counter and hand it a retry budget it has not earned.
func TestOnlyExhaustedDeliveriesReplay(t *testing.T) {
	w := newWebhookEnv(t)

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	w.endpoint(t, server.URL)
	w.emit(t, events.TypePaymentSucceeded, "pay_1")

	if _, err := w.dispatcher.DispatchOnce(w.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}
	if _, err := w.worker(16).DeliverOnce(w.ctx); err != nil {
		t.Fatalf("DeliverOnce = %v", err)
	}

	succeeded := w.deliveries(t, "")[0]
	if err := w.webhookRepo.ReplayDelivery(w.ctx, testMerchant, succeeded.ID); !errors.Is(err, webhooks.ErrNotReplayable) {
		t.Errorf("replaying a succeeded delivery = %v, want ErrNotReplayable", err)
	}
}

// A 410 means "stop sending". Continuing to retry for 35 hours after being told that is
// both rude and pointless.
func TestGoneDisablesTheEndpointImmediately(t *testing.T) {
	w := newWebhookEnv(t)

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusGone)
	}))
	defer server.Close()

	endpoint, _ := w.endpoint(t, server.URL)
	w.emit(t, events.TypePaymentSucceeded, "pay_1")

	if _, err := w.dispatcher.DispatchOnce(w.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}
	if _, err := w.worker(16).DeliverOnce(w.ctx); err != nil {
		t.Fatalf("DeliverOnce = %v", err)
	}

	after, err := w.webhookRepo.GetEndpoint(w.ctx, testMerchant, endpoint.ID)
	if err != nil {
		t.Fatalf("GetEndpoint = %v", err)
	}
	if after.Status != webhooks.EndpointDisabled {
		t.Errorf("status = %s, want disabled after a 410", after.Status)
	}
	if got := w.deliveries(t, "")[0].State; got != webhooks.StateDisabled {
		t.Errorf("delivery state = %s, want disabled", got)
	}
}

// Two workers against one queue must not deliver the same event twice. This is what
// SKIP LOCKED buys, and it is the reason no broker is involved.
func TestTwoWorkersNeverDeliverTheSameEventTwice(t *testing.T) {
	w := newWebhookEnv(t)

	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		time.Sleep(10 * time.Millisecond) // widen the window for an overlap
		rw.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	w.endpoint(t, server.URL)

	const eventCount = 25
	for i := 0; i < eventCount; i++ {
		w.emit(t, events.TypePaymentSucceeded, "pay_"+string(rune('a'+i%26)))
	}
	if _, err := w.dispatcher.DispatchOnce(w.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			worker := w.worker(16)
			for j := 0; j < 5; j++ {
				if _, err := worker.DeliverOnce(context.Background()); err != nil {
					t.Errorf("DeliverOnce = %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got := hits.Load(); got != eventCount {
		t.Errorf("the endpoint was called %d times for %d events; SKIP LOCKED should make each delivery exclusive",
			got, eventCount)
	}

	for _, d := range w.deliveries(t, "") {
		if d.State != webhooks.StateSucceeded {
			t.Errorf("delivery %s is %s, want succeeded", d.ID, d.State)
		}
		if d.Attempt != 1 {
			t.Errorf("delivery %s was attempted %d times, want 1", d.ID, d.Attempt)
		}
	}
}

// A worker that dies mid-POST leaves a leased row. The sweeper reclaims it, which is
// exactly why delivery is at-least-once and consumers must dedupe on the event id.
func TestExpiredLeasesAreReclaimed(t *testing.T) {
	w := newWebhookEnv(t)

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	w.endpoint(t, server.URL)
	w.emit(t, events.TypePaymentSucceeded, "pay_1")

	if _, err := w.dispatcher.DispatchOnce(w.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}

	// Claim without delivering, as a worker that died immediately afterwards would.
	claimed, err := w.webhookRepo.ClaimDue(w.ctx, 10, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatalf("ClaimDue = %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("claimed %d deliveries, want 1", len(claimed))
	}

	// While the lease is live nobody else can take it.
	again, err := w.webhookRepo.ClaimDue(w.ctx, 10, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatalf("ClaimDue = %v", err)
	}
	if len(again) != 0 {
		t.Errorf("a leased delivery was claimed again: %d rows", len(again))
	}

	// Expire the lease and sweep.
	if _, err := w.db.Pool().Exec(w.ctx,
		`UPDATE webhook_deliveries SET leased_until = now() - interval '1 hour' WHERE id = $1`,
		claimed[0].ID); err != nil {
		t.Fatalf("expiring the lease: %v", err)
	}

	reclaimed, err := w.webhookRepo.ReclaimExpiredLeases(w.ctx, time.Now().UTC())
	if err != nil {
		t.Fatalf("ReclaimExpiredLeases = %v", err)
	}
	if reclaimed != 1 {
		t.Errorf("reclaimed %d deliveries, want 1", reclaimed)
	}

	if _, err := w.worker(16).DeliverOnce(w.ctx); err != nil {
		t.Fatalf("DeliverOnce = %v", err)
	}
	if got := w.deliveries(t, "")[0].State; got != webhooks.StateSucceeded {
		t.Errorf("state = %s, want succeeded after the reclaim", got)
	}
}

// Fan-out is idempotent, so a dispatcher that crashed part way through expanding an
// event cannot produce a second delivery to the same endpoint on its next run.
func TestFanOutIsIdempotent(t *testing.T) {
	w := newWebhookEnv(t)

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	endpoint, _ := w.endpoint(t, server.URL)
	event := w.emit(t, events.TypePaymentSucceeded, "pay_1")

	if _, err := w.dispatcher.DispatchOnce(w.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}

	// Re-run the expansion by hand, as a crashed dispatcher's retry would.
	if err := w.webhookRepo.EnqueueDeliveries(w.ctx, []*webhooks.Delivery{{
		ID:         id.New(id.PrefixDelivery),
		EventID:    event.ID,
		EndpointID: endpoint.ID,
		MerchantID: testMerchant,
	}}); err != nil {
		t.Fatalf("re-enqueue = %v", err)
	}

	if got := len(w.deliveries(t, "")); got != 1 {
		t.Errorf("%d deliveries after a repeated fan-out, want 1", got)
	}
}

// Only subscribed endpoints receive an event, and a filter typo is silence rather than
// a flood — which is why filters are validated at registration.
func TestFiltersSelectEndpoints(t *testing.T) {
	w := newWebhookEnv(t)

	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		rw.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	payments, _ := w.endpoint(t, server.URL, "payment.*")
	transfers, _ := w.endpoint(t, server.URL, "transfer.posted")

	w.emit(t, events.TypePaymentSucceeded, "pay_1")
	if _, err := w.dispatcher.DispatchOnce(w.ctx); err != nil {
		t.Fatalf("DispatchOnce = %v", err)
	}

	if got := len(w.deliveriesFor(t, payments.ID)); got != 1 {
		t.Errorf("the payment.* endpoint got %d deliveries, want 1", got)
	}
	if got := len(w.deliveriesFor(t, transfers.ID)); got != 0 {
		t.Errorf("the transfer.posted endpoint got %d deliveries for a payment event, want 0", got)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

func (w *webhookEnv) emit(t *testing.T, typ events.Type, resourceID string) *events.Event {
	t.Helper()

	var e *events.Event
	if err := w.inTx(func(ctx context.Context) error {
		var err error
		e, err = w.recorder.Record(ctx, testMerchant, typ, resourceID, map[string]any{
			"id": resourceID, "object": "payment",
		})
		return err
	}); err != nil {
		t.Fatalf("recording an event: %v", err)
	}
	return e
}

// deliveries returns every delivery for the first endpoint, optionally filtered.
func (w *webhookEnv) deliveries(t *testing.T, state webhooks.DeliveryState) []*webhooks.Delivery {
	t.Helper()

	var endpointID string
	if err := w.db.Pool().QueryRow(w.ctx,
		`SELECT id FROM webhook_endpoints ORDER BY created_at LIMIT 1`).Scan(&endpointID); err != nil {
		t.Fatalf("finding an endpoint: %v", err)
	}

	out, err := w.webhookRepo.ListDeliveries(w.ctx, endpointID, state, 100)
	if err != nil {
		t.Fatalf("ListDeliveries = %v", err)
	}
	return out
}

func (w *webhookEnv) deliveriesFor(t *testing.T, endpointID string) []*webhooks.Delivery {
	t.Helper()

	out, err := w.webhookRepo.ListDeliveries(w.ctx, endpointID, "", 100)
	if err != nil {
		t.Fatalf("ListDeliveries = %v", err)
	}
	return out
}

func readBody(r *http.Request) []byte {
	defer func() { _ = r.Body.Close() }()

	body, _ := io.ReadAll(r.Body)
	return body
}
