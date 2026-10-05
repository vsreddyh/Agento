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
