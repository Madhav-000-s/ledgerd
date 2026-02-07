package id

import (
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewHasPrefixAndLength(t *testing.T) {
	got := New(PrefixAccount)

	if !strings.HasPrefix(got, "acct_") {
		t.Errorf("New(PrefixAccount) = %q, want an acct_ prefix", got)
	}
	if len(got) != len("acct_")+encodedLen {
		t.Errorf("New(PrefixAccount) = %q, length %d, want %d", got, len(got), len("acct_")+encodedLen)
	}
	if !Valid(got, PrefixAccount) {
		t.Errorf("Valid(%q, acct) = false", got)
	}
	// A transaction ID must not validate as an account ID.
	if Valid(got, PrefixTransaction) {
		t.Errorf("Valid(%q, txn) = true, want false", got)
	}
}

func TestValid(t *testing.T) {
	good := New(PrefixTransaction)

	tests := []struct {
		name   string
		s      string
		prefix string
		want   bool
	}{
		{"well formed", good, PrefixTransaction, true},
		{"wrong prefix", good, PrefixAccount, false},
		{"no prefix expected", NewULID(), "", true},
		{"too short", "txn_01HQ", PrefixTransaction, false},
		{"too long", good + "X", PrefixTransaction, false},
		{"excluded letter I", "txn_" + strings.Repeat("I", encodedLen), PrefixTransaction, false},
		{"excluded letter L", "txn_" + strings.Repeat("L", encodedLen), PrefixTransaction, false},
		{"excluded letter O", "txn_" + strings.Repeat("O", encodedLen), PrefixTransaction, false},
		{"excluded letter U", "txn_" + strings.Repeat("U", encodedLen), PrefixTransaction, false},
		{"lower case", strings.ToLower(good), PrefixTransaction, false},
		{"empty", "", PrefixTransaction, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Valid(tc.s, tc.prefix); got != tc.want {
				t.Errorf("Valid(%q, %q) = %v, want %v", tc.s, tc.prefix, got, tc.want)
			}
		})
	}
}

// Sorting IDs as strings must sort them by creation time. This is the property that
// lets an index on the ID column double as a time-ordered index, and it is the whole
// reason for using ULIDs rather than UUIDs.
func TestLexicographicOrderMatchesTimeOrder(t *testing.T) {
	const n = 500

	ids := make([]string, n)
	for i := range ids {
		ids[i] = New(PrefixEntry)
		if i%100 == 0 {
			time.Sleep(2 * time.Millisecond) // cross a few millisecond boundaries
		}
	}

	sorted := append([]string(nil), ids...)
	sort.Strings(sorted)

	for i := range ids {
		if ids[i] != sorted[i] {
			t.Fatalf("IDs do not sort into issue order; position %d is %q, want %q", i, sorted[i], ids[i])
		}
	}
}

// Within a single millisecond the counter increments rather than redrawing, so IDs
// issued in the same millisecond still sort in issue order.
func TestMonotonicWithinMillisecond(t *testing.T) {
	frozen := time.UnixMilli(1_700_000_000_000)
	g := &generator{
		now:      func() time.Time { return frozen },
		randRead: func(b []byte) (int, error) { return len(b), nil }, // all zeros
	}

	prev := g.encode()
	for i := 0; i < 1000; i++ {
		next := g.encode()
		if next <= prev {
			t.Fatalf("iteration %d: %q is not greater than %q", i, next, prev)
		}
		prev = next
	}
}

// A clock that steps backwards must not produce IDs that go backwards with it.
func TestClockGoingBackwards(t *testing.T) {
	now := time.UnixMilli(1_700_000_000_000)
	g := &generator{
		now:      func() time.Time { return now },
		randRead: func(b []byte) (int, error) { return len(b), nil },
	}

	first := g.encode()
	now = now.Add(-time.Second) // NTP correction, VM suspend, whatever
	second := g.encode()

	if second <= first {
		t.Errorf("after a backward clock step, %q is not greater than %q", second, first)
	}
}

func TestUniqueUnderConcurrency(t *testing.T) {
	const (
		goroutines = 32
		perG       = 500
	)

	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		all = make(map[string]struct{}, goroutines*perG)
	)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := make([]string, perG)
			for j := range local {
				local[j] = New(PrefixTransaction)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, s := range local {
				if _, dup := all[s]; dup {
					t.Errorf("duplicate identifier %q", s)
				}
				all[s] = struct{}{}
			}
		}()
	}
	wg.Wait()

	if len(all) != goroutines*perG {
		t.Errorf("got %d distinct IDs, want %d", len(all), goroutines*perG)
	}
}

func TestTimestampRoundTrip(t *testing.T) {
	before := time.Now().UTC().Truncate(time.Millisecond)
	got := New(PrefixEvent)
	after := time.Now().UTC()

	ts, err := Timestamp(got)
	if err != nil {
		t.Fatalf("Timestamp(%q) = %v", got, err)
	}
	if ts.Before(before) || ts.After(after.Add(time.Millisecond)) {
		t.Errorf("Timestamp(%q) = %v, want between %v and %v", got, ts, before, after)
	}
}

func TestTimestampExactValue(t *testing.T) {
	want := time.UnixMilli(1_700_000_000_000).UTC()
	g := &generator{
		now:      func() time.Time { return want },
		randRead: func(b []byte) (int, error) { return len(b), nil },
	}

	got, err := Timestamp(g.new(PrefixAccount))
	if err != nil {
		t.Fatalf("Timestamp = %v", err)
	}
	if !got.Equal(want) {
		t.Errorf("Timestamp = %v, want %v", got, want)
	}
}

func TestTimestampRejectsMalformed(t *testing.T) {
	for _, s := range []string{"", "acct_", "acct_TOOSHORT", strings.Repeat("!", encodedLen)} {
		if _, err := Timestamp(s); err == nil {
			t.Errorf("Timestamp(%q) = nil, want an error", s)
		}
	}
}

// The alphabet must exclude the four characters Crockford drops, or an ID read aloud
// becomes ambiguous.
func TestAlphabetExcludesConfusableLetters(t *testing.T) {
	if len(crockford) != 32 {
		t.Fatalf("alphabet has %d characters, want 32", len(crockford))
	}
	for _, c := range []byte("ILOU") {
		if strings.IndexByte(crockford, c) >= 0 {
			t.Errorf("alphabet contains %q", c)
		}
	}
	seen := map[byte]bool{}
	for i := 0; i < len(crockford); i++ {
		if seen[crockford[i]] {
			t.Errorf("alphabet repeats %q", crockford[i])
		}
		seen[crockford[i]] = true
	}
}
