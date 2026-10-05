package main

// Per-source rate limiting for health-api (#176).
//
// Why this exists: the API is network-reachable, authed by a single shared
// password, and every route is a thin pass-through to a shared production
// MongoDB. A stuck client retrying in a loop — or anyone who has the password —
// turns that into an unauthenticated-rate DoS against the data backend. There
// was no counter, no backoff and no lockout anywhere in the server.
//
// A token bucket rather than a fixed window, because the failure mode here is a
// BURST: a retry loop fires as fast as the socket allows, and a fixed window
// either lets that burst through or throttles a legitimate catch-up after a
// brief outage. The bucket allows a burst up to the bucket size and then settles
// to the refill rate.
//
// In-memory on purpose. This is a single-process service whose whole state is
// already in memory (see gateway.SessionStore); a shared store would add a
// dependency and a failure mode to defend against a threat that needs a lot of
// requests. The consequence is that a restart resets the buckets, which is the
// right trade: the limiter exists to smooth accidents, not to withstand a
// determined attacker with the password.

import (
	"hash/fnv"
	"net"
	"net/http"
	"strconv"
	"strings"
	stdsync "sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

const (
	// defaultBurst is how many requests may arrive at once before throttling.
	// The app does a handful on a cold start (tasks, health, projects, skills),
	// and a sync can fan out into more, so this is sized for "a real burst", not
	// for a single request.
	defaultBurst = 40
	// defaultRefill is the sustained rate, per second. ~120/min sustained: well
	// above anything the app or the agent does in normal use, low enough that a
	// runaway loop is the only thing that reaches it.
	defaultRefill = 2.0
	// idleTTL is how long an unused bucket is kept. Without it the map grows
	// with every distinct source address forever, which is a slower leak but a
	// leak.
	idleTTL = 10 * time.Minute
)

type bucket struct {
	tokens   float64
	last     time.Time
	lastSeen time.Time
}

type limiter struct {
	mu      stdsync.Mutex
	buckets map[string]*bucket
	burst   float64
	refill  float64
	now     func() time.Time
}

func newLimiter(burst float64, refill float64) *limiter {
	return &limiter{
		buckets: map[string]*bucket{},
		burst:   burst,
		refill:  refill,
		now:     time.Now,
	}
}

// allow consumes one token for key. It reports whether the request may proceed and,
// when it may not, how long the caller should wait (always at least a second, so a
// client cannot be told "retry in 0" and spin).
func (l *limiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.sweepLocked(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst}
		l.buckets[key] = b
	}
	// Refill for the elapsed time, then consume. Refilling before spending is what
	// makes this a bucket rather than a counter that resets once per window.
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * l.refill
		if b.tokens > l.burst {
			b.tokens = l.burst
		}
	}
	b.last = now
	b.lastSeen = now

	if b.tokens < 1 {
		wait := time.Duration((1 - b.tokens) / l.refill * float64(time.Second))
		if wait < time.Second {
			wait = time.Second
		}
		return false, wait
	}
	b.tokens--
	return true, 0
}

// sweepLocked drops buckets untouched for idleTTL. Called under the lock, and
// only when the map has grown enough to be worth walking — this runs on every
// request and a personal service does not need a linear scan per call.
func (l *limiter) sweepLocked(now time.Time) {
	if len(l.buckets) < 256 {
		return
	}
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) > idleTTL {
			delete(l.buckets, k)
		}
	}
}

// sourceKey identifies a caller for throttling. The address is preferred because
// it is the thing an unauthenticated flood varies; the token is folded in so two
// callers behind one address (app and agent on the same host) do not share a
// budget they each need in full.
func sourceKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if tok := bearerToken(r); tok != "" {
		// Hashed, not stored: the limiter's map is long-lived and there is no
		// reason for it to hold anything derived from the password.
		return host + "|" + shortHash(tok)
	}
	return host
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

// shortHash is a stable, non-cryptographic digest. The limiter only needs buckets
// to be distinct per token, never to be recoverable from them — it is a long-lived
// in-memory map and has no business holding anything derived from the password.
func shortHash(s string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return strconv.FormatUint(h.Sum64(), 36)
}

// rateLimit wraps h with the limiter. Exported-for-test seam: newLimiter takes
// burst/refill and a clock, so the bucket's behaviour can be asserted without
// sleeping.
func rateLimit(l *limiter, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ok, wait := l.allow(sourceKey(r)); !ok {
			secs := int(wait.Seconds())
			if secs < 1 {
				secs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			writeJSON(w, http.StatusTooManyRequests, bson.M{
				"detail": "too many requests — slow down and retry after " + strconv.Itoa(secs) + "s",
			})
			return
		}
		h.ServeHTTP(w, r)
	})
}
