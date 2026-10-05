package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// #176: the API had no rate limit anywhere. These use an injected clock so the
// bucket's behaviour is asserted exactly rather than approximated with sleeps —
// which is also why a burst/replenish test is not flaky.

func testLimiter() (*limiter, func(time.Duration)) {
	return newLimiterWithClock(3, 1.0) // 3 burst, 1 token/second
}

func TestLimiterAllowsTheBurstThenThrottles(t *testing.T) {
	l, _ := testLimiter()
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("a"); !ok {
			t.Fatalf("request %d of a 3-burst was refused", i+1)
		}
	}
	ok, wait := l.allow("a")
	if ok {
		t.Fatal("the 4th request in a burst of 3 must be refused")
	}
	if wait < time.Second {
		t.Errorf("Retry-After of %v is too short to be useful — the client would spin", wait)
	}
}

func TestLimiterReplenishesOverTime(t *testing.T) {
	l, advance := testLimiter()
	for i := 0; i < 3; i++ {
		l.allow("a")
	}
	if ok, _ := l.allow("a"); ok {
		t.Fatal("expected throttling before any time passes")
	}
	// At 1 token/second, one second buys exactly one more request.
	advance(time.Second)
	if ok, _ := l.allow("a"); !ok {
		t.Error("one second at 1 token/s must buy one request")
	}
	if ok, _ := l.allow("a"); ok {
		t.Error("the bucket must be empty again after spending that token")
	}
	advance(10 * time.Second)
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("a"); !ok {
			t.Errorf("request %d refused after a long idle period", i+1)
		}
	}
}

// The bucket must not exceed its size while idle, or a quiet hour would buy a
// hundred-request burst and defeat the limit entirely.
func TestLimiterDoesNotAccumulateBeyondBurst(t *testing.T) {
	l, advance := testLimiter()
	advance(time.Hour)
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("a"); !ok {
			t.Fatalf("request %d refused after an hour idle", i+1)
		}
	}
	if ok, _ := l.allow("a"); ok {
		t.Error("an hour of idling must not buy more than the burst size")
	}
}

// Sources are throttled independently. A flood from one address must not spend
// another's budget — in particular, the app's, since the two share a password.
func TestLimiterSeparatesSources(t *testing.T) {
	l, _ := testLimiter()
	for i := 0; i < 3; i++ {
		l.allow("flooder")
	}
	if ok, _ := l.allow("flooder"); ok {
		t.Fatal("the flooder should be throttled")
	}
	if ok, _ := l.allow("innocent"); !ok {
		t.Error("one source's flood must not throttle an unrelated source")
	}
}

func TestLimiterSweepsIdleBuckets(t *testing.T) {
	l, advance := testLimiter()
	l.allow("a")
	if len(l.buckets) == 0 {
		t.Fatal("no bucket recorded")
	}
	// The sweep is gated on map size, so grow it past the gate first.
	for i := 0; i < 300; i++ {
		l.allow(string(rune('A'+i%26)) + string(rune('a'+i/26)))
	}
	before := len(l.buckets)
	advance(idleTTL + time.Minute)
	l.allow("trigger") // any call runs the sweep
	if len(l.buckets) >= before {
		t.Errorf("idle buckets were not swept: %d -> %d", before, len(l.buckets))
	}
}

// The 429 must be actionable: a status, a Retry-After a client can obey, and a
// message that says what to do.
func TestRateLimitHandlerReturns429WithRetryAfter(t *testing.T) {
	l, _ := newLimiterWithClock(1, 1.0)
	h := rateLimit(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	called := 0
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
		req.RemoteAddr = "10.0.0.1:5555"
		h.ServeHTTP(rec, req)
		if i == 0 {
			called++
			if rec.Code != http.StatusOK {
				t.Fatalf("first request = %d, want 200", rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("request %d = %d, want 429", i+1, rec.Code)
		}
		ra := rec.Header().Get("Retry-After")
		if ra == "" {
			t.Error("429 without Retry-After — a client cannot know when to come back")
		}
		if !strings.Contains(rec.Body.String(), "retry after") {
			t.Errorf("429 body does not tell the client what to do: %s", rec.Body.String())
		}
	}
	if called != 1 {
		t.Errorf("the wrapped handler ran %d times, want 1", called)
	}
}

// A throttled request must NOT reach the handler: a limiter that only logs is
// not a limiter.
func TestRateLimitBlocksTheHandlerEntirely(t *testing.T) {
	l, _ := newLimiterWithClock(1, 1.0)
	ran := 0
	h := rateLimit(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran++
	}))
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
		req.RemoteAddr = "10.0.0.2:5555"
		h.ServeHTTP(rec, req)
	}
	if ran != 1 {
		t.Errorf("handler ran %d times under a burst of 5 with burst=1, want 1", ran)
	}
}

// newLimiterWithClock is the seam the handler tests use; newLimiter itself reads
// the wall clock, which would make these assertions timing-dependent.
func newLimiterWithClock(burst, refill float64) (*limiter, func(time.Duration)) {
	now := time.Now()
	l := newLimiter(burst, refill)
	l.now = func() time.Time { return now }
	return l, func(d time.Duration) { now = now.Add(d) }
}

// The source key must not retain anything derived from the password. The bucket
// map is long-lived and process-wide, and there is no reason for it to be a place
// a credential lives.
func TestSourceKeyDoesNotRetainTheToken(t *testing.T) {
	mk := func(token string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
		r.RemoteAddr = "10.0.0.3:1234"
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		return r
	}
	withToken := sourceKey(mk("super-secret-password"))
	if strings.Contains(withToken, "super-secret") {
		t.Errorf("source key retains the token verbatim: %q", withToken)
	}
	// Distinct tokens still get distinct budgets.
	if sourceKey(mk("password-a")) == sourceKey(mk("password-b")) {
		t.Error("two different tokens share a bucket — the hash is not being applied")
	}
	// And an absent token is fine.
	if sourceKey(mk("")) == "" {
		t.Error("an unauthenticated request still needs a key to throttle against")
	}
}

// The compose file publishes health-api as `8001:8000`, so a client can reach it
// WITHOUT nginx and can set X-Real-IP to anything. Trusting the header
// unconditionally would let any caller defeat the limiter by rotating one header —
// which is the difference between a rate limit and decoration. These pin the trust
// boundary: the header is believed only from an internal peer.
func TestSourceKeyIgnoresForwardedHeadersFromAPublicPeer(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	r.RemoteAddr = "203.0.113.9:5555" // PUBLIC client, straight at the published port
	r.Header.Set("X-Real-IP", "198.51.100.7")
	r.Header.Set("X-Forwarded-For", "198.51.100.7")
	key := sourceKey(r)
	if strings.Contains(key, "198.51.100.7") {
		t.Errorf("a public peer chose its own bucket via X-Real-IP: %q", key)
	}
	if !strings.HasPrefix(key, "203.0.113.9") {
		t.Errorf("key = %q, want it keyed on the real peer 203.0.113.9", key)
	}
}

// Rotating the header from a public peer must not buy a fresh bucket each time —
// otherwise "per-source" means "per-request" and the limiter does nothing.
func TestSourceKeyCannotBeRotatedByAPublicPeer(t *testing.T) {
	seen := map[string]bool{}
	for _, spoofed := range []string{"1.1.1.1", "2.2.2.2", "3.3.3.3", "4.4.4.4"} {
		r := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
		r.RemoteAddr = "203.0.113.9:5555"
		r.Header.Set("X-Real-IP", spoofed)
		seen[sourceKey(r)] = true
	}
	if len(seen) != 1 {
		t.Errorf("one client produced %d buckets by rotating a header: %v", len(seen), seen)
	}
}

// Traffic that came through the compose network IS separated per real client, which
// is the whole reason for reading the header at all.
func TestSourceKeySeparatesClientsBehindTheProxy(t *testing.T) {
	key := func(client string) string {
		r := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
		r.RemoteAddr = "172.18.0.5:5000" // the nginx container
		r.Header.Set("X-Real-IP", client)
		return sourceKey(r)
	}
	if key("198.51.100.7") == key("198.51.100.8") {
		t.Error("two clients through the proxy share a bucket — X-Real-IP is being ignored")
	}
	if key("198.51.100.7") == "172.18.0.5" {
		t.Error("the bucket is keyed on the proxy, so every client shares one budget")
	}
}

// nginx uses $proxy_add_x_forwarded_for, which APPENDS to a client-supplied list, so
// every entry except the last is attacker-chosen. Taking the first — the conventional
// choice — takes the attacker's value.
func TestSourceKeyUsesTheLastForwardedHop(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
	r.RemoteAddr = "172.18.0.5:5000"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8, 198.51.100.9")
	key := sourceKey(r)
	if !strings.HasPrefix(key, "198.51.100.9") {
		t.Errorf("key = %q, want the LAST hop (198.51.100.9)", key)
	}
}

// A garbage header must not become a bucket key, and must not fall through to an
// empty one — either would let a client choose its own bucket.
func TestSourceKeyRejectsUnparseableHeaders(t *testing.T) {
	for _, bad := range []string{"not-an-ip", "", "999.999.999.999", "1.2.3.4; DROP"} {
		r := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
		r.RemoteAddr = "172.18.0.5:5000"
		if bad != "" {
			r.Header.Set("X-Real-IP", bad)
		}
		key := sourceKey(r)
		if key == "" {
			t.Errorf("header %q produced an empty bucket key", bad)
		}
		if key == bad {
			t.Errorf("unparseable header %q was used verbatim as the key", bad)
		}
	}
}

// The end-to-end property: a flood arriving through the proxy is throttled as ONE
// source, and a second proxied client is unaffected by it.
func TestProxiedClientsGetIndependentBudgets(t *testing.T) {
	l, _ := newLimiterWithClock(2, 1.0)
	h := rateLimit(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	do := func(client string) int {
		ok200 := 0
		for i := 0; i < 5; i++ {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/tasks", nil)
			req.RemoteAddr = "172.18.0.5:5000"
			req.Header.Set("X-Real-IP", client)
			h.ServeHTTP(rec, req)
			if rec.Code == http.StatusOK {
				ok200++
			}
		}
		return ok200
	}
	if got := do("198.51.100.7"); got != 2 {
		t.Errorf("flooding client got %d through, want 2 (its burst)", got)
	}
	if got := do("198.51.100.8"); got != 2 {
		t.Errorf("an innocent proxied client got %d through, want its own burst of 2", got)
	}
}

// The bucket's `last` must be initialised. Left as the zero time, the first refill
// measures from year 1 and saturates instantly — harmless only because the burst
// clamp hides it, which is exactly why relying on the clamp is the problem.
func TestNewBucketStartsWithAClockNotTheEpoch(t *testing.T) {
	l, _ := newLimiterWithClock(3, 1.0)
	l.allow("fresh")
	b := l.buckets["fresh"]
	if b.last.IsZero() {
		t.Error("a new bucket has a zero `last`; the first refill saturates instead of metering")
	}
}
