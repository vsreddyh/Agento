package calendar

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agento/internal/connectors"

	"golang.org/x/oauth2"
)

// Stub Calendar wire: list/get shapes only, no auth. Records list bounds.
func stubCalendar(t *testing.T, bounds *map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/calendars/primary/events") && !strings.Contains(r.URL.Path, "/events/"):
			if bounds != nil {
				*bounds = map[string]string{"timeMin": q.Get("timeMin"), "timeMax": q.Get("timeMax"), "max": q.Get("maxResults")}
			}
			fmt.Fprint(w, `{"items":[{"id":"e1","summary":"Standup",`+
				`"start":{"dateTime":"2026-10-26T09:00:00Z"},"end":{"dateTime":"2026-10-26T09:30:00Z"},`+
				`"attendees":[{"email":"a@example.com "}],"htmlLink":"https://cal.example/e1"}]}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events/e1"):
			fmt.Fprint(w, `{"id":"e1","summary":"All hands",`+
				`"start":{"date":"2026-10-27"},"end":{"date":"2026-10-28"}}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events/enull"):
			fmt.Fprint(w, `null`)
		default:
			t.Errorf("unexpected calendar call %s %s", r.Method, r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

func testClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"})
	c, err := NewClientWithToken(context.Background(), src, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestListReturnsUpcoming(t *testing.T) {
	bounds := map[string]string{}
	srv := stubCalendar(t, &bounds)
	defer srv.Close()
	rows, err := testClient(t, srv.URL).List(context.Background(), "", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if bounds["timeMin"] == "" || bounds["timeMax"] == "" || bounds["max"] != "10" {
		t.Errorf("list bounds = %v, want timeMin/timeMax/10", bounds)
	}
	if len(rows) != 1 {
		t.Fatalf("list = %d rows, want 1", len(rows))
	}
	e := rows[0]
	if e.Summary != "Standup" || e.Start != "2026-10-26T09:00:00Z" {
		t.Errorf("event = %+v, want Standup at 09:00", e)
	}
	if len(e.Attendees) != 1 || e.Attendees[0] != "a@example.com" {
		t.Errorf("attendees = %v, want trimmed single", e.Attendees)
	}
	if e.Link != "https://cal.example/e1" {
		t.Errorf("link = %q", e.Link)
	}
}

func TestGetReadsAllDay(t *testing.T) {
	srv := stubCalendar(t, nil)
	defer srv.Close()
	e, err := testClient(t, srv.URL).Get(context.Background(), "", "e1")
	if err != nil {
		t.Fatal(err)
	}
	if !e.AllDay || e.Start != "2026-10-27" {
		t.Errorf("event = %+v, want all-day 2026-10-27", e)
	}
	if _, err := testClient(t, srv.URL).Get(context.Background(), "", ""); err == nil {
		t.Fatal("empty id should fail")
	}
}

func TestConvertGuardsNil(t *testing.T) {
	ev := convert(nil)
	if ev.ID != "" || ev.Summary != "" || len(ev.Attendees) != 0 {
		t.Errorf("convert(nil) = %+v, want zero Event", ev)
	}
}

func TestGetNullEventFailsClosed(t *testing.T) {
	srv := stubCalendar(t, nil)
	defer srv.Close()
	if _, err := testClient(t, srv.URL).Get(context.Background(), "", "enull"); err == nil {
		t.Fatal("null upstream event should fail, not panic")
	}
}

func TestListRejectsBackwardWindow(t *testing.T) {
	srv := stubCalendar(t, nil)
	defer srv.Close()
	now := time.Now()
	if _, err := testClient(t, srv.URL).List(context.Background(), "", now, now.Add(-time.Hour), 0); err == nil {
		t.Fatal("end-before-start should fail")
	}
}

// Drafts never touch the wire: table-driven validation only.
func TestDraftValidation(t *testing.T) {
	for _, tc := range []struct {
		name                           string
		summary, desc, loc, start, end string
		attendees                      []string
		wantErr                        string
		wantAllDay                     bool
	}{
		{"timed", "1:1", "", "", "2026-10-26T09:00:00Z", "2026-10-26T09:30:00Z", nil, "", false},
		{"allday", "Off", "", "", "2026-10-27", "2026-10-28", []string{" a@x.com ", ""}, "", true},
		{"missing summary", "", "", "", "2026-10-26T09:00:00Z", "2026-10-26T10:00:00Z", nil, "summary", false},
		{"missing start", "1:1", "", "", "", "2026-10-26T10:00:00Z", nil, "start and an end", false},
		{"bad start", "1:1", "", "", "monday", "2026-10-26T10:00:00Z", nil, "RFC3339", false},
		{"nonexistent date", "1:1", "", "", "2026-02-30", "2026-03-01", nil, "match in kind", false},
		{"end before start", "1:1", "", "", "2026-10-26T10:00:00Z", "2026-10-26T09:00:00Z", nil, "after start", false},
		{"allday end before start", "1:1", "", "", "2026-10-28", "2026-10-27", nil, "after start", false},
		{"allday single day is zero-day", "1:1", "", "", "2026-10-28", "2026-10-28", nil, "after start", false},
		{"bad attendee", "1:1", "", "", "2026-10-26T09:00:00Z", "2026-10-26T10:00:00Z", []string{"not-an-email"}, "attendee", false},
		{"non-ascii attendee", "1:1", "", "", "2026-10-26T09:00:00Z", "2026-10-26T10:00:00Z", []string{"caf\u00e9@x.com"}, "attendee", false},
		{"multi attendee", "1:1", "", "", "2026-10-26T09:00:00Z", "2026-10-26T10:00:00Z", []string{"a@x.com b@y.com"}, "attendee", false},
		{"newline attendee", "1:1", "", "", "2026-10-26T09:00:00Z", "2026-10-26T10:00:00Z", []string{"a@x.com\nBcc: v@y.com"}, "attendee", false},
		{"display-name attendee", "1:1", "", "", "2026-10-26T09:00:00Z", "2026-10-26T10:00:00Z", []string{"John Doe <a@x.com>"}, "attendee", false},
		{"long summary", strings.Repeat("s", MaxSummary+1), "", "", "2026-10-26T09:00:00Z", "2026-10-26T10:00:00Z", nil, "summary too long", false},
		{"mixed kinds", "1:1", "", "", "2026-10-27", "2026-10-26T10:00:00Z", nil, "match in kind", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Draft(tc.summary, tc.desc, tc.loc, tc.start, tc.end, tc.attendees)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if d.AllDay != tc.wantAllDay {
				t.Errorf("allday = %v, want %v", d.AllDay, tc.wantAllDay)
			}
		})
	}
}

func TestDraftTrimsAndCleans(t *testing.T) {
	d, err := Draft("  1:1 ", "", " Room ", "2026-10-26T09:00:00Z", "2026-10-26T09:30:00Z", []string{"a@x.com", " ", "b@x.com"})
	if err != nil {
		t.Fatal(err)
	}
	if d.Summary != "1:1" || d.Location != "Room" {
		t.Errorf("draft not trimmed: %+v", d)
	}
	if len(d.Attendees) != 2 {
		t.Errorf("attendees = %v, want 2 cleaned", d.Attendees)
	}
	dup, err := Draft("1:1", "", "", "2026-10-26T09:00:00Z", "2026-10-26T09:30:00Z", []string{"a@x.com", "a@x.com", "b@x.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(dup.Attendees) != 2 || dup.Attendees[0] != "a@x.com" || dup.Attendees[1] != "b@x.com" {
		t.Errorf("attendees = %v, want deduplicated in order", dup.Attendees)
	}
	fold, err := Draft("1:1", "", "", "2026-10-26T09:00:00Z", "2026-10-26T09:30:00Z", []string{"A@x.com", "a@x.com"})
	if err != nil {
		t.Fatal(err)
	}
	if len(fold.Attendees) != 1 || fold.Attendees[0] != "A@x.com" {
		t.Errorf("attendees = %v, want case-folded dedupe keeping first spelling", fold.Attendees)
	}
	big, err := Draft("Big", strings.Repeat("x", MaxDescription+10), "", "2026-10-26T09:00:00Z", "2026-10-26T10:00:00Z", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(big.Description)); n != MaxDescription+1 {
		t.Errorf("description runes = %d, want %d+marker", n, MaxDescription)
	}
	if !big.Truncated {
		t.Error("over-cap draft must report truncated")
	}
}

// Construction needs no grant: a disconnected deploy must still boot, and
// the grant resolves per tool call with reconnect guidance.
func TestNewClientNeedsNoGrant(t *testing.T) {
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	key, err := connectors.KeyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	srv := stubCalendar(t, nil)
	defer srv.Close()
	c, err := NewClient(connectors.NewMemoryStore(),
		&connectors.OAuthConfig{ClientID: "cid", ClientSecret: "sec", Key: key}, srv.URL, nil)
	if err != nil {
		t.Fatalf("construction without a grant should succeed: %v", err)
	}
	if _, err := c.List(context.Background(), "", time.Time{}, time.Time{}, 0); err == nil ||
		!strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("grant-less call = %v, want reconnect guidance", err)
	}
}
