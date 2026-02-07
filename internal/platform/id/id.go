// Package id generates prefixed, lexicographically sortable identifiers.
//
// Identifiers are ULIDs: a 48-bit millisecond timestamp followed by 80 bits of
// randomness, rendered in Crockford base32. Two properties matter here.
//
// Sorting a set of IDs as strings sorts them by creation time, which means an index on
// the ID column is also a time-ordered index and a cursor over IDs is a stable cursor
// over history. A random UUID would force every "most recent entries" query to sort by
// a separate timestamp column and would scatter index writes across the whole B-tree.
//
// Within a single millisecond the random component is incremented rather than redrawn,
// so IDs issued in the same millisecond still sort in issue order instead of arbitrarily.
package id

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Prefixes. Every identifier carries the kind of thing it names, so that a value pasted
// into a bug report or a WHERE clause is self-describing and a transaction ID can never
// be mistaken for an account ID.
const (
	PrefixAccount     = "acct"
	PrefixTransaction = "txn"
	PrefixEntry       = "en"
	PrefixEvent       = "evt"
	PrefixPayment     = "pay"
	PrefixRefund      = "re"
	PrefixEndpoint    = "we"
	PrefixDelivery    = "whd"
	PrefixIdempotency = "idem"
	PrefixRequest     = "req"
	PrefixAPIKey      = "ak"
	PrefixSecret      = "whsec"
)

// crockford is the base32 alphabet, excluding I, L, O and U so that a human reading an
// identifier aloud or copying it by hand cannot confuse it with 1, 0 or V.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// encodedLen is ceil(128/5): 26 characters covering 130 bits, the top two of which are
// always zero.
const encodedLen = 26

// generator holds the state needed for same-millisecond monotonicity.
type generator struct {
	mu       sync.Mutex
	lastMS   uint64
	lastRand [10]byte
	now      func() time.Time
	randRead func([]byte) (int, error)
}

var std = &generator{now: time.Now, randRead: rand.Read}

// New returns a new identifier with the given prefix, e.g. "acct_01HQ8Z...".
func New(prefix string) string { return std.new(prefix) }

// NewULID returns a bare ULID with no prefix.
func NewULID() string { return std.encode() }

func (g *generator) new(prefix string) string {
	if prefix == "" {
		return g.encode()
	}
	return prefix + "_" + g.encode()
}

func (g *generator) encode() string {
	g.mu.Lock()
	defer g.mu.Unlock()

	ms := uint64(g.now().UnixMilli())

	switch {
	case ms > g.lastMS:
		g.lastMS = ms
		if _, err := g.randRead(g.lastRand[:]); err != nil {
			// crypto/rand does not fail on any supported platform; if it ever does,
			// continuing would mean issuing predictable identifiers.
			panic(fmt.Sprintf("id: crypto/rand failed: %v", err))
		}
	default:
		// Same millisecond, or a clock that went backwards. Increment the random
		// component so IDs stay unique and in issue order. Reusing lastMS on a backward
		// clock step keeps them monotonic rather than briefly reversing.
		if !increment(&g.lastRand) {
			// 80 bits exhausted within one millisecond. Unreachable in practice:
			// it would take 2^80 identifiers.
			panic("id: monotonic counter exhausted within a millisecond")
		}
	}

	var b [16]byte
	// Big-endian 48-bit timestamp in the first six bytes, so byte order and sort order
	// agree.
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], g.lastMS<<16)
	copy(b[0:6], ts[0:6])
	copy(b[6:16], g.lastRand[:])

	return encodeBase32(b)
}

// increment adds one to the 80-bit counter, reporting false on overflow.
func increment(b *[10]byte) bool {
	for i := len(b) - 1; i >= 0; i-- {
		b[i]++
		if b[i] != 0 {
			return true
		}
	}
	return false
}

// encodeBase32 renders 128 bits as 26 Crockford base32 characters, most significant
// first, padding the leading group with two zero bits.
func encodeBase32(b [16]byte) string {
	var out [encodedLen]byte
	for i := 0; i < encodedLen; i++ {
		// Bit position, in the 128-bit stream, of this group's most significant bit.
		// The first group starts two bits "before" the stream; those bits read as zero.
		base := i*5 - 2

		var v byte
		for j := 0; j < 5; j++ {
			p := base + j
			if p < 0 {
				continue
			}
			if b[p/8]&(1<<uint(7-p%8)) != 0 {
				v |= 1 << uint(4-j)
			}
		}
		out[i] = crockford[v]
	}
	return string(out[:])
}

// Valid reports whether s is a well-formed identifier with the given prefix.
func Valid(s, prefix string) bool {
	if prefix != "" {
		want := prefix + "_"
		if !strings.HasPrefix(s, want) {
			return false
		}
		s = s[len(want):]
	}
	if len(s) != encodedLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if strings.IndexByte(crockford, s[i]) < 0 {
			return false
		}
	}
	return true
}

// Timestamp recovers the creation time encoded in an identifier. It is used by the
// reaper and by cursor pagination, and it is why IDs are ULIDs rather than UUIDs.
func Timestamp(s string) (time.Time, error) {
	if i := strings.LastIndexByte(s, '_'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) != encodedLen {
		return time.Time{}, fmt.Errorf("id: %q is not a ULID", s)
	}

	// Decode the first ten characters, which hold the 48-bit timestamp plus the two
	// leading pad bits.
	var ms uint64
	for i := 0; i < 10; i++ {
		v := strings.IndexByte(crockford, s[i])
		if v < 0 {
			return time.Time{}, fmt.Errorf("id: %q contains a character outside the alphabet", s)
		}
		ms = ms<<5 | uint64(v)
	}
	// 10 groups of 5 bits = 50 bits; drop the two leading pad bits.
	ms &= (1 << 48) - 1

	return time.UnixMilli(int64(ms)).UTC(), nil
}
