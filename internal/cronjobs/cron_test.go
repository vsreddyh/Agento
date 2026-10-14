package cronjobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) *Schedule {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("Parse(%q): %v", expr, err)
	}
	return s
}

func at(day int, hour, min int) time.Time {
	return time.Date(2026, time.October, day, hour, min, 0, 0, IST)
}

func TestParseShapes(t *testing.T) {
	for _, expr := range []string{
		"0 9 * * *", "*/15 8-18 * * 1-5", "0 0 1 JAN,MAR mon",
		"@hourly", "@daily", "@weekly", "@monthly", "30 6 * * SUN",
	} {
		if _, err := Parse(expr); err != nil {
			t.Errorf("Parse(%q): %v", expr, err)
		}
	}
	for _, expr := range []string{
		"", "0 9 * *", "* * * * * *", "61 * * * *", "0 24 * * *",
		"0 9 * * FUNDAY", "0 9 * * */0", "nope",
	} {
		if _, err := Parse(expr); err == nil {
			t.Errorf("Parse(%q) should fail", expr)
		}
	}
}

func TestMatchesDaily(t *testing.T) {
	s := mustParse(t, "30 9 * * *")
	if !s.Matches(at(14, 9, 30)) {
		t.Error("should match 09:30")
	}
	if s.Matches(at(14, 9, 31)) || s.Matches(at(14, 10, 30)) {
		t.Error("should not match off-minutes")
	}
}

func TestMatchesWeekdays(t *testing.T) {
	// 2026-10-14 is a Wednesday.
	s := mustParse(t, "0 9 * * 1-5")
	if !s.Matches(at(14, 9, 0)) {
		t.Error("Wednesday should match")
	}
	if s.Matches(at(17, 9, 0)) { // Saturday
		t.Error("Saturday should not match")
	}
}

func TestMatchesSundayBothSpellings(t *testing.T) {
	// 2026-10-18 is a Sunday.
	for _, expr := range []string{"0 9 * * 0", "0 9 * * 7", "0 9 * * SUN"} {
		if !mustParse(t, expr).Matches(at(18, 9, 0)) {
			t.Errorf("%q should match Sunday", expr)
		}
	}
}

func TestDayRuleEitherOr(t *testing.T) {
	// dom restricted + dow restricted: either matches.
	s := mustParse(t, "0 0 15 * 3")
	if !s.Matches(time.Date(2026, time.October, 15, 0, 0, 0, 0, IST)) { // Thu 15th
		t.Error("15th should match by dom")
	}
	if !s.Matches(time.Date(2026, time.October, 21, 0, 0, 0, 0, IST)) { // Wed 21st
		t.Error("Wednesday should match by dow")
	}
	if s.Matches(time.Date(2026, time.October, 16, 0, 0, 0, 0, IST)) { // Fri 16th
		t.Error("Fri 16th should match neither")
	}
}

func TestLatestAndNext(t *testing.T) {
	s := mustParse(t, "0 9 * * *")
	now := at(14, 12, 0)
	if got := s.LatestBefore(now); !got.Equal(at(14, 9, 0)) {
		t.Errorf("LatestBefore = %v", got)
	}
	if got := s.Next(now); !got.Equal(at(15, 9, 0)) {
		t.Errorf("Next = %v", got)
	}
	if got := s.Next(at(14, 9, 0)); !got.Equal(at(15, 9, 0)) {
		t.Errorf("Next is strictly after: %v", got)
	}
}

func TestAliases(t *testing.T) {
	if got := mustParse(t, "@daily").Next(at(14, 12, 0)); !got.Equal(at(15, 0, 0)) {
		t.Errorf("@daily Next = %v", got)
	}
	if got := mustParse(t, "@hourly").Next(at(14, 12, 30)); !got.Equal(at(14, 13, 0)) {
		t.Errorf("@hourly Next = %v", got)
	}
}

func TestBareClientRoundTrip(t *testing.T) {
	var gotSession, gotUA, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("x-opencode-session")
		gotUA = r.Header.Get("User-Agent")
		gotAuth = r.Header.Get("Authorization")
		var req struct {
			Model    string `json:"model"`
			Stream   bool   `json:"stream"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if req.Model != "mimo-v2.6-flash" || req.Stream {
			t.Errorf("bad shape: %+v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "  hi  "}}},
		})
	}))
	defer srv.Close()
	c := &BareClient{APIKey: "k", Model: "opencode-go/mimo-v2.6-flash", BaseURL: srv.URL, HTTP: srv.Client()}
	text, err := c.Run(context.Background(), "nudge", "agento-cron-abc")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if text != "hi" {
		t.Errorf("got %q", text)
	}
	if gotSession != "agento-cron-abc" || !strings.HasPrefix(gotUA, "agento-cron/") || gotAuth != "Bearer k" {
		t.Errorf("headers: session=%q ua=%q auth=%q", gotSession, gotUA, gotAuth)
	}
}

func TestBareClientErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = w.Write([]byte(`{"error":{"message":"pay up","type":"billing"}}`))
	}))
	defer srv.Close()
	c := &BareClient{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTP: srv.Client()}
	if _, err := c.Run(context.Background(), "x", "s"); err == nil ||
		!strings.Contains(err.Error(), "402") || !strings.Contains(err.Error(), "pay up") {
		t.Errorf("got %v", err)
	}
	if _, err := (&BareClient{Model: "m"}).Run(context.Background(), "x", "s"); err == nil {
		t.Error("missing key should fail")
	}
}
