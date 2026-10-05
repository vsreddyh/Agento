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
		// `last` is set, not left zero. With a zero `last`, the elapsed time below is
		// measured from year 1 and the refill instantly saturates the bucket — which
		// happens to be harmless only because the burst cap clamps it. Relying on a
		// clamp to rescue an uninitialised field is how the field silently stops being
		// the thing you think it is.
		b = &bucket{tokens: l.burst, last: now, lastSeen: now}
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

// peerIsInternal reports whether the immediate TCP peer is on a private or
// loopback network, i.e. plausibly our own nginx rather than the public internet.
func peerIsInternal(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Not an IP at all (a unix socket, a test harness). Refuse to trust the
		// header: the safe answer is the one that cannot be spoofed.
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// clientIP is the address to throttle by, preferring what the reverse proxy
// observed over what this process can see.
//
// WHY THIS IS NOT JUST "trust X-Real-IP": the compose file publishes health-api
// directly as `8001:8000`, so a client can reach it WITHOUT going through nginx and
// can set X-Real-IP to anything it likes. Trusting the header unconditionally would
// hand every direct client a fresh bucket per request by rotating a header — a
// rate limit that any caller can switch off with one line of curl.
//
// So the header is trusted only when the peer is internal, which is true for
// traffic that came through the compose network and false for a public client
// hitting the published port directly. Nginx overwrites X-Real-IP with $remote_addr
// rather than passing the client's value through, which is what makes this safe on
// our side of the wire.
func clientIP(r *http.Request) string {
	if peerIsInternal(r) {
		if v := realIP(r); v != "" {
			return v
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// realIP reads the proxy-observed client address. X-Real-IP first: nginx SETS it to
// $remote_addr, overwriting whatever the client sent. X-Forwarded-For is only
// consulted as a fallback and only its LAST hop, because nginx uses
// $proxy_add_x_forwarded_for, which APPENDS to a client-supplied list — every entry
// before the last is attacker-chosen, so taking the first (the conventional choice)
// would be taking the attacker's value.
func realIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		if net.ParseIP(v) != nil {
			return v
		}
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		parts := strings.Split(xff, ",")
		last := strings.TrimSpace(parts[len(parts)-1])
		if net.ParseIP(last) != nil {
			return last
		}
	}
	return ""
}

// sourceKey identifies a caller for throttling: the client address, plus a hash of
// the bearer token so two callers sharing one address (app and agent on the same
// host) do not share a budget they each need in full.
func sourceKey(r *http.Request) string {
	host := clientIP(r)
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
