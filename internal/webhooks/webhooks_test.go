package webhooks

import (
	"context"
	"errors"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Madhav-000-s/ledgerd/pkg/webhookverify"
)

// ── backoff ─────────────────────────────────────────────────────────────────

// The per-attempt caps stated in DESIGN.md §8.4. The doc's numbers are outputs of this
// schedule, so if the schedule changes the doc is wrong rather than the test.
func TestBackoffSchedule(t *testing.T) {
	b := DefaultBackoff

	want := []time.Duration{
		10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second,
		160 * time.Second, 320 * time.Second, 640 * time.Second, 1280 * time.Second,
		2560 * time.Second, 5120 * time.Second, 10240 * time.Second, 20480 * time.Second,
		21600 * time.Second, 21600 * time.Second, 21600 * time.Second, 21600 * time.Second,
	}

	got := b.Schedule(MaxAttempts)
	if len(got) != len(want) {
		t.Fatalf("schedule has %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("interval %d = %s, want %s", i, got[i], want[i])
		}
	}
}

// The design doc claims ~35.4 hours worst case and ~17.7 expected. Those are computed,
// not estimated, so they are checked.
func TestBackoffWindows(t *testing.T) {
	b := DefaultBackoff

	worst := b.WorstCaseWindow(MaxAttempts)
	expected := b.ExpectedWindow(MaxAttempts)

	if hours := worst.Hours(); hours < 35.3 || hours > 35.5 {
		t.Errorf("worst-case window = %.2fh, want about 35.4h", hours)
	}
	if hours := expected.Hours(); hours < 17.6 || hours > 17.8 {
		t.Errorf("expected window = %.2fh, want about 17.7h", hours)
	}
	if expected*2 != worst {
		t.Errorf("expected window is not half the worst case: %s vs %s", expected, worst)
	}
}

// Full jitter means a uniform draw from the whole interval, not the interval plus a
// little noise. After an outage, thousands of deliveries come due at once; anything less
// than full randomization lets them arrive as a herd and knock the endpoint back over.
func TestDelayIsFullJitter(t *testing.T) {
	b := Backoff{Base: 10 * time.Second, Cap: 6 * time.Hour, Rand: rand.New(rand.NewSource(1))}

	const attempt = 5
	interval := b.Interval(attempt)

	var (
		low, high int
		minSeen   = interval
		maxSeen   time.Duration
	)
	for i := 0; i < 2000; i++ {
		d := b.Delay(attempt)
		if d < 0 || d > interval {
			t.Fatalf("delay %s is outside [0, %s]", d, interval)
		}
		if d < interval/2 {
			low++
		} else {
			high++
		}
		if d < minSeen {
			minSeen = d
		}
		if d > maxSeen {
			maxSeen = d
		}
	}

	// A roughly even split across the interval. A schedule that only jittered the top
	// of the range would fail this badly.
	if low < 700 || high < 700 {
		t.Errorf("draws are not spread across the interval: %d below the midpoint, %d above", low, high)
	}
	// And the draw must actually reach both ends.
	if minSeen > interval/10 {
		t.Errorf("smallest delay seen was %s, never approaching zero", minSeen)
	}
	if maxSeen < interval*9/10 {
		t.Errorf("largest delay seen was %s, never approaching the interval %s", maxSeen, interval)
	}
}

func TestBackoffNeverOverflows(t *testing.T) {
	b := DefaultBackoff

	// Far past the retry budget, where 2^attempt would have overflowed long ago.
	for _, attempt := range []int{0, 16, 64, 1000, 1 << 20} {
		if d := b.Interval(attempt); d <= 0 || d > b.Cap {
			t.Errorf("Interval(%d) = %s, want a positive value no greater than the cap", attempt, d)
		}
		if d := b.Delay(attempt); d < 0 {
			t.Errorf("Delay(%d) = %s, want a non-negative delay", attempt, d)
		}
	}
}

// ── signing ─────────────────────────────────────────────────────────────────

func newCipher(t *testing.T) *Cipher {
	t.Helper()

	c, err := NewCipher(strings.Repeat("ab", 32))
	if err != nil {
		t.Fatalf("NewCipher = %v", err)
	}
	return c
}

func TestCipherRoundTrip(t *testing.T) {
	c := newCipher(t)

	const secret = "whsec_0123456789abcdef"
	sealed, err := c.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt = %v", err)
	}
	if strings.Contains(string(sealed), secret) {
		t.Error("the ciphertext contains the plaintext")
	}

	got, err := c.Decrypt(sealed)
	if err != nil {
		t.Fatalf("Decrypt = %v", err)
	}
	if got != secret {
		t.Errorf("Decrypt = %q, want %q", got, secret)
	}

	// Two encryptions of the same secret must differ, or the nonce is being reused —
	// which for GCM is catastrophic rather than merely untidy.
	again, err := c.Encrypt(secret)
	if err != nil {
		t.Fatalf("Encrypt = %v", err)
	}
	if string(sealed) == string(again) {
		t.Error("two encryptions produced identical ciphertext; the nonce is not random")
	}
}

func TestCipherRejectsTamperedCiphertext(t *testing.T) {
	c := newCipher(t)

	sealed, err := c.Encrypt("whsec_secret")
	if err != nil {
		t.Fatalf("Encrypt = %v", err)
	}
	sealed[len(sealed)-1] ^= 0xff

	if _, err := c.Decrypt(sealed); !errors.Is(err, ErrDecrypt) {
		t.Errorf("Decrypt of tampered ciphertext = %v, want ErrDecrypt", err)
	}
}

func TestCipherRejectsBadKeys(t *testing.T) {
	for _, key := range []string{"", "abcd", strings.Repeat("z", 64), strings.Repeat("ab", 16)} {
		if _, err := NewCipher(key); !errors.Is(err, ErrBadSecretKey) {
			t.Errorf("NewCipher(%q) = %v, want ErrBadSecretKey", key, err)
		}
	}
}

// A payload signed here must verify with the reference verifier a consumer would use.
// If these two ever disagree, every consumer is broken and the sender looks fine.
func TestSignatureVerifiesWithTheReferenceVerifier(t *testing.T) {
	now := time.Unix(1_753_776_000, 0).UTC()
	signer := NewSigner(func() time.Time { return now })

	body := []byte(`{"id":"evt_1","type":"payment.succeeded"}`)
	secret := &Secret{Plaintext: "whsec_test", Active: true}

	header, ts, err := signer.Sign(body, []*Secret{secret})
	if err != nil {
		t.Fatalf("Sign = %v", err)
	}
	if !ts.Equal(now) {
		t.Errorf("timestamp = %v, want %v", ts, now)
	}

	if err := webhookverify.VerifyAt(header, body, secret.Plaintext, webhookverify.DefaultTolerance, now); err != nil {
		t.Fatalf("the reference verifier rejected our own signature: %v", err)
	}
}

func TestTamperedBodyFailsVerification(t *testing.T) {
	now := time.Unix(1_753_776_000, 0).UTC()
	signer := NewSigner(func() time.Time { return now })

	body := []byte(`{"amount":100}`)
	secret := &Secret{Plaintext: "whsec_test", Active: true}

	header, _, err := signer.Sign(body, []*Secret{secret})
	if err != nil {
		t.Fatalf("Sign = %v", err)
	}

	tampered := []byte(`{"amount":900}`)
	if err := webhookverify.VerifyAt(header, tampered, secret.Plaintext, 0, now); !errors.Is(err, webhookverify.ErrNoMatch) {
		t.Errorf("a tampered body verified: %v", err)
	}
}

// An old signature must be refused, or anyone who captures one can replay it forever.
func TestStaleTimestampIsRejected(t *testing.T) {
	signedAt := time.Unix(1_753_776_000, 0).UTC()
	signer := NewSigner(func() time.Time { return signedAt })

	body := []byte(`{"id":"evt_1"}`)
	secret := &Secret{Plaintext: "whsec_test", Active: true}
	header, _, _ := signer.Sign(body, []*Secret{secret})

	sixMinutesLater := signedAt.Add(6 * time.Minute)
	if err := webhookverify.VerifyAt(header, body, secret.Plaintext, 0, sixMinutesLater); !errors.Is(err, webhookverify.ErrTooOld) {
		t.Errorf("a six-minute-old signature verified: %v", err)
	}

	// Inside the window it is still fine.
	fourMinutesLater := signedAt.Add(4 * time.Minute)
	if err := webhookverify.VerifyAt(header, body, secret.Plaintext, 0, fourMinutesLater); err != nil {
		t.Errorf("a four-minute-old signature was rejected: %v", err)
	}

	// A timestamp far in the future is as suspicious as one far in the past.
	before := signedAt.Add(-6 * time.Minute)
	if err := webhookverify.VerifyAt(header, body, secret.Plaintext, 0, before); !errors.Is(err, webhookverify.ErrTooOld) {
		t.Errorf("a signature from six minutes in the future verified: %v", err)
	}
}

// Both secrets must validate during a rotation overlap, which is what makes rotation
// zero-downtime rather than a synchronized cutover.
func TestRotationOverlapValidatesBothSecrets(t *testing.T) {
	now := time.Unix(1_753_776_000, 0).UTC()
	signer := NewSigner(func() time.Time { return now })

	body := []byte(`{"id":"evt_1"}`)
	old := &Secret{Plaintext: "whsec_old", Active: true}
	fresh := &Secret{Plaintext: "whsec_new", Active: true}

	header, _, err := signer.Sign(body, []*Secret{old, fresh})
	if err != nil {
		t.Fatalf("Sign = %v", err)
	}

	if strings.Count(header, "v1=") != 2 {
		t.Errorf("header carries %d signatures, want 2: %s", strings.Count(header, "v1="), header)
	}
	for name, secret := range map[string]string{"old": old.Plaintext, "new": fresh.Plaintext} {
		if err := webhookverify.VerifyAt(header, body, secret, 0, now); err != nil {
			t.Errorf("the %s secret did not verify during the overlap: %v", name, err)
		}
	}
	// An unrelated secret still must not.
	if err := webhookverify.VerifyAt(header, body, "whsec_other", 0, now); !errors.Is(err, webhookverify.ErrNoMatch) {
		t.Errorf("an unrelated secret verified: %v", err)
	}
}

func TestSignRequiresASecret(t *testing.T) {
	signer := NewSigner(nil)

	if _, _, err := signer.Sign([]byte(`{}`), nil); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("Sign with no secrets = %v, want ErrSecretNotFound", err)
	}
	if _, _, err := signer.Sign([]byte(`{}`), []*Secret{{Active: true}}); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("Sign with an undecrypted secret = %v, want ErrSecretNotFound", err)
	}
}

func TestParseSignatureHeader(t *testing.T) {
	for _, bad := range []string{"", "garbage", "v1=abcd", "t=notanumber,v1=ab", "t=123"} {
		if _, err := webhookverify.Parse(bad); err == nil {
			t.Errorf("Parse(%q) = nil, want an error", bad)
		}
	}

	sig, err := webhookverify.Parse("t=1753776000,v1=aabb,v1=ccdd")
	if err != nil {
		t.Fatalf("Parse = %v", err)
	}
	if len(sig.V1) != 2 {
		t.Errorf("got %d signatures, want 2", len(sig.V1))
	}
	if sig.Timestamp.Unix() != 1_753_776_000 {
		t.Errorf("timestamp = %v", sig.Timestamp)
	}
}

// An unparseable signature among several must not stop the valid ones being checked.
func TestParseSkipsMalformedSignatures(t *testing.T) {
	sig, err := webhookverify.Parse("t=1753776000,v1=nothex,v1=aabb")
	if err != nil {
		t.Fatalf("Parse = %v", err)
	}
	if len(sig.V1) != 1 {
		t.Errorf("got %d usable signatures, want 1", len(sig.V1))
	}
}

// ── SSRF ────────────────────────────────────────────────────────────────────

func TestBlockedAddresses(t *testing.T) {
	blocked := []string{
		"127.0.0.1", "127.1.2.3", "0.0.0.0",
		"169.254.169.254", // the cloud metadata service
		"10.0.0.1", "172.16.5.4", "192.168.1.1",
		"100.64.0.1",       // CGNAT
		"::1",              // IPv6 loopback
		"fc00::1",          // unique local
		"fe80::1",          // link-local
		"::ffff:127.0.0.1", // IPv4-mapped loopback
		"224.0.0.1",        // multicast
		"255.255.255.255",
	}
	for _, addr := range blocked {
		if !IsBlockedIP(net.ParseIP(addr)) {
			t.Errorf("IsBlockedIP(%s) = false, want true", addr)
		}
	}

	allowed := []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"}
	for _, addr := range allowed {
		if IsBlockedIP(net.ParseIP(addr)) {
			t.Errorf("IsBlockedIP(%s) = true, want false", addr)
		}
	}

	if !IsBlockedIP(nil) {
		t.Error("IsBlockedIP(nil) = false; an unparseable address must not be treated as safe")
	}
}

func TestValidateURL(t *testing.T) {
	strict := NewClient(ClientOptions{})

	bad := map[string]string{
		"plain http":     "http://example.com/hook",
		"loopback":       "https://127.0.0.1/hook",
		"metadata":       "https://169.254.169.254/latest/meta-data/",
		"private":        "https://10.0.0.5/hook",
		"file scheme":    "file:///etc/passwd",
		"no host":        "https:///hook",
		"credentials":    "https://user:pass@example.com/hook",
		"unknown scheme": "gopher://example.com/",
	}
	for name, raw := range bad {
		if err := strict.ValidateURL(raw); err == nil {
			t.Errorf("%s: ValidateURL(%q) = nil, want an error", name, raw)
		}
	}

	if err := strict.ValidateURL("https://merchant.example.com/webhooks"); err != nil {
		t.Errorf("a valid endpoint was rejected: %v", err)
	}

	// The local-development escape hatch.
	loose := NewClient(ClientOptions{AllowInsecure: true, AllowPrivateIPs: true})
	if err := loose.ValidateURL("http://127.0.0.1:8080/hook"); err != nil {
		t.Errorf("with the guards disabled, ValidateURL = %v", err)
	}
}

// The check that matters: at dial time, on the address actually being connected to.
// Validating the URL at registration leaves DNS rebinding wide open, because the name
// resolved to something public then and resolves to loopback now.
func TestDialTimeGuardStopsDNSRebinding(t *testing.T) {
	// A resolver that answers with loopback for a perfectly innocent-looking name.
	rebinding := NewClient(ClientOptions{
		Resolver: func(_ context.Context, host string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
		},
	})

	// The URL passes registration validation: the host is a name, not a literal.
	if err := rebinding.ValidateURL("https://totally-legit.example.com/hook"); err != nil {
		t.Fatalf("ValidateURL = %v; the point of this test is that registration cannot catch it", err)
	}

	resp := rebinding.Deliver(context.Background(), "https://totally-legit.example.com/hook", []byte(`{}`), nil)
	if resp.Err == nil {
		t.Fatal("the delivery succeeded against a name that resolved to loopback")
	}
	if !strings.Contains(resp.Err.Error(), "private or reserved") {
		t.Errorf("error = %v, want the private-address guard", resp.Err)
	}
}

func TestDialTimeGuardBlocksMetadataService(t *testing.T) {
	client := NewClient(ClientOptions{
		Resolver: func(_ context.Context, _ string) ([]net.IPAddr, error) {
			return []net.IPAddr{{IP: net.ParseIP("169.254.169.254")}}, nil
		},
	})

	resp := client.Deliver(context.Background(), "https://metadata.example.com/", []byte(`{}`), nil)
	if resp.Err == nil || !strings.Contains(resp.Err.Error(), "private or reserved") {
		t.Errorf("the metadata service was reachable: %v", resp.Err)
	}
}

// A redirect is the simplest way around a validated URL: answer 302 and point at
// 169.254.169.254.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var reachedSecond bool

	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedSecond = true
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL, http.StatusFound)
	}))
	defer first.Close()

	client := NewClient(ClientOptions{AllowInsecure: true, AllowPrivateIPs: true})
	resp := client.Deliver(context.Background(), first.URL, []byte(`{}`), nil)

	if reachedSecond {
		t.Error("the redirect was followed")
	}
	if !errors.Is(resp.Err, ErrRedirect) {
		t.Errorf("error = %v, want ErrRedirect", resp.Err)
	}
}

func TestDeliverSendsSignedBodyUnchanged(t *testing.T) {
	var (
		gotBody      []byte
		gotSignature string
		gotTopic     string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotSignature = r.Header.Get(webhookverify.Header)
		gotTopic = r.Header.Get("Webhook-Topic")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(ClientOptions{AllowInsecure: true, AllowPrivateIPs: true})
	body := []byte(`{"id":"evt_1","type":"payment.succeeded"}`)

	now := time.Now().UTC()
	signer := NewSigner(func() time.Time { return now })
	secret := &Secret{Plaintext: "whsec_test", Active: true}
	header, _, err := signer.Sign(body, []*Secret{secret})
	if err != nil {
		t.Fatalf("Sign = %v", err)
	}

	resp := client.Deliver(context.Background(), server.URL, body, map[string]string{
		webhookverify.Header: header,
		"Webhook-Topic":      "payment.succeeded",
	})
	if !resp.Succeeded() {
		t.Fatalf("delivery failed: status %d, err %v", resp.StatusCode, resp.Err)
	}

	if string(gotBody) != string(body) {
		t.Errorf("the endpoint received %q, want %q", gotBody, body)
	}
	if gotTopic != "payment.succeeded" {
		t.Errorf("topic header = %q", gotTopic)
	}
	// The signature must verify against the bytes that actually arrived.
	if err := webhookverify.VerifyAt(gotSignature, gotBody, secret.Plaintext, 0, now); err != nil {
		t.Errorf("the received body did not verify: %v", err)
	}
}

func TestDeliverBoundsTheResponseBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("x", 1<<20)))
	}))
	defer server.Close()

	client := NewClient(ClientOptions{AllowInsecure: true, AllowPrivateIPs: true, MaxResponseBytes: 1024})
	resp := client.Deliver(context.Background(), server.URL, []byte(`{}`), nil)

	if len(resp.Body) > 1024 {
		t.Errorf("read %d bytes of the response body, want no more than 1024", len(resp.Body))
	}
	if resp.Succeeded() {
		t.Error("a 500 was reported as success")
	}
}

// ── retry classification ────────────────────────────────────────────────────

func TestClassify(t *testing.T) {
	tests := []struct {
		name    string
		resp    Response
		attempt int
		want    Outcome
	}{
		{"200", Response{StatusCode: 200}, 0, OutcomeSucceeded},
		{"204", Response{StatusCode: 204}, 0, OutcomeSucceeded},
		{"410 gone stops immediately", Response{StatusCode: 410}, 0, OutcomeDisabled},
		{"429 retries", Response{StatusCode: 429}, 0, OutcomeRetry},
		{"503 retries", Response{StatusCode: 503}, 0, OutcomeRetry},
		{"404 retries, a deploy may be in progress", Response{StatusCode: 404}, 0, OutcomeRetry},
		{"400 retries", Response{StatusCode: 400}, 0, OutcomeRetry},
		{"timeout retries", Response{Err: context.DeadlineExceeded}, 0, OutcomeRetry},
		{"last attempt exhausts", Response{StatusCode: 500}, MaxAttempts - 1, OutcomeExhausted},
		{"past the budget exhausts", Response{StatusCode: 500}, MaxAttempts, OutcomeExhausted},
		{"410 on the last attempt still disables", Response{StatusCode: 410}, MaxAttempts - 1, OutcomeDisabled},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.resp, tc.attempt, MaxAttempts); got != tc.want {
				t.Errorf("Classify = %v, want %v", got, tc.want)
			}
		})
	}
}

// An endpoint that says "not for another 90 seconds" is telling us something useful
// about its own load; ignoring it is how a struggling service gets pushed over.
func TestRetryAfter(t *testing.T) {
	now := time.Unix(1_753_776_000, 0).UTC()
	capacity := 6 * time.Hour

	d, ok := RetryAfter("90", now, capacity)
	if !ok || d != 90*time.Second {
		t.Errorf("RetryAfter(90) = %s, %v", d, ok)
	}

	// Capped, so an endpoint cannot park a delivery for a week.
	d, ok = RetryAfter("999999", now, capacity)
	if !ok || d != capacity {
		t.Errorf("RetryAfter(999999) = %s, want the cap %s", d, capacity)
	}

	// HTTP-date form.
	d, ok = RetryAfter(now.Add(2*time.Minute).Format(http.TimeFormat), now, capacity)
	if !ok || d < time.Minute || d > 2*time.Minute {
		t.Errorf("RetryAfter(http-date) = %s, %v", d, ok)
	}

	for _, bad := range []string{"", "soon", "-5"} {
		if _, ok := RetryAfter(bad, now, capacity); ok {
			t.Errorf("RetryAfter(%q) reported usable", bad)
		}
	}
}

func TestEndpointWants(t *testing.T) {
	active := &Endpoint{Status: EndpointActive, EnabledEvents: []string{"payment.*"}}
	if !active.Wants("payment.succeeded") {
		t.Error("payment.* did not match payment.succeeded")
	}
	if active.Wants("transfer.posted") {
		t.Error("payment.* matched transfer.posted")
	}

	// A disabled endpoint receives nothing, whatever it is subscribed to.
	disabled := &Endpoint{Status: EndpointDisabled, EnabledEvents: []string{"*"}}
	if disabled.Wants("payment.succeeded") {
		t.Error("a disabled endpoint still wants events")
	}
}
