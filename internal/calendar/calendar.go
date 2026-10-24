// Package calendar lists upcoming events and builds draft events through
// the official Calendar API, using the connector grant the OAuth layer
// sealed into the store (#316). Reads are free; writes are draft-only:
// create_event_draft builds an agent-side object (validated, sent nowhere),
// while create_event/send_invite are refused by the relay before they reach
// this process (see connectors.IsDraftOnlySend) — the grant carries
// readonly, so the token is incapable of writing even if the relay check
// vanished.
package calendar

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agento/internal/connectors"

	"golang.org/x/oauth2"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

const (
	// DefaultLimit answers "what's coming up" without a number.
	DefaultLimit = 10
	// MaxLimit bounds one list response.
	MaxLimit = 50
	// MaxDescription caps one event description; over-cap text is
	// truncated and reported, never silently cut.
	MaxDescription = 5000
	// DefaultWindow bounds an unfiltered list: upcoming means the next 30
	// days, not the whole calendar history.
	DefaultWindow = 30 * 24 * time.Hour
)

// Client calls the Calendar API as the connected grant. No grant (or a
// dead one) is a reconnect error, never a secrets prompt.
type Client struct {
	svc *calendar.Service
}

// NewClient builds a Client from the stored connector grant, refreshing
// and persisting on expiry (see connectors.GrantClient). Endpoint
// overrides the Calendar API base URL — httptest in tests, empty in prod.
func NewClient(ctx context.Context, store connectors.Store, cfg *connectors.OAuthConfig, endpoint string) (*Client, error) {
	httpClient, err := connectors.GrantClient(ctx, store, cfg, connectors.Calendar)
	if err != nil {
		return nil, err
	}
	opts := []option.ClientOption{option.WithHTTPClient(httpClient)}
	if endpoint != "" {
		opts = append(opts, option.WithEndpoint(endpoint))
	}
	svc, err := calendar.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{svc: svc}, nil
}

// NewClientWithToken builds a Client around an explicit token source (tests
// that drive the Calendar wire without a connector store).
func NewClientWithToken(ctx context.Context, src oauth2.TokenSource, endpoint string) (*Client, error) {
	opts := []option.ClientOption{option.WithTokenSource(src)}
	if endpoint != "" {
		opts = append(opts, option.WithEndpoint(endpoint))
	}
	svc, err := calendar.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{svc: svc}, nil
}

// Event is the agent-facing shape: when + what, no sync metadata.
type Event struct {
	ID          string   `json:"id,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Description string   `json:"description,omitempty"`
	Location    string   `json:"location,omitempty"`
	Start       string   `json:"start,omitempty"`
	End         string   `json:"end,omitempty"`
	AllDay      bool     `json:"all_day,omitempty"`
	Attendees   []string `json:"attendees,omitempty"`
	Link        string   `json:"link,omitempty"`
	// Truncated reports an over-cap description cut.
	Truncated bool `json:"truncated,omitempty"`
}

// List returns upcoming events (default calendar "primary", default window
// the next 30 days), soonest first.
func (c *Client) List(ctx context.Context, calendarID string, from, to time.Time, limit int) ([]Event, error) {
	calendarID = strings.TrimSpace(calendarID)
	if calendarID == "" {
		calendarID = "primary"
	}
	now := time.Now()
	if from.IsZero() {
		from = now
	}
	if to.IsZero() {
		to = now.Add(DefaultWindow)
	}
	if !to.After(from) {
		return nil, fmt.Errorf("calendar: list needs an end after its start")
	}
	res, err := c.svc.Events.List(calendarID).
		TimeMin(from.Format(time.RFC3339)).
		TimeMax(to.Format(time.RFC3339)).
		MaxResults(int64(boundLimit(limit))).
		SingleEvents(true).
		OrderBy("startTime").
		Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("calendar: list events: %w", err)
	}
	out := make([]Event, 0, len(res.Items))
	for _, e := range res.Items {
		out = append(out, convert(e))
	}
	return out, nil
}

// Get returns one event by id.
func (c *Client) Get(ctx context.Context, calendarID, id string) (*Event, error) {
	calendarID = strings.TrimSpace(calendarID)
	if calendarID == "" {
		calendarID = "primary"
	}
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("calendar: get needs an event id")
	}
	e, err := c.svc.Events.Get(calendarID, strings.TrimSpace(id)).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("calendar: get event: %w", err)
	}
	ev := convert(e)
	return &ev, nil
}

// DraftEvent is an agent-side draft: validated and normalised, sent
// nowhere. The agent presents it to the user; actual creation waits for the
// approval-gate flow that covers calendar writes.
type DraftEvent struct {
	Summary     string   `json:"summary"`
	Description string   `json:"description,omitempty"`
	Location    string   `json:"location,omitempty"`
	Start       string   `json:"start"`
	End         string   `json:"end"`
	AllDay      bool     `json:"all_day,omitempty"`
	Attendees   []string `json:"attendees,omitempty"`
}

// Draft validates event fields and returns the draft WITHOUT calling
// Google: a readonly grant cannot write, and this tool must work on
// exactly the grant the connect flow mints. Start/end accept RFC3339
// date-times or YYYY-MM-DD dates (both ends must match in kind).
func Draft(summary, description, location, start, end string, attendees []string) (*DraftEvent, error) {
	if strings.TrimSpace(summary) == "" {
		return nil, fmt.Errorf("calendar: draft needs a summary")
	}
	start = strings.TrimSpace(start)
	end = strings.TrimSpace(end)
	if start == "" || end == "" {
		return nil, fmt.Errorf("calendar: draft needs a start and an end")
	}
	allDay := false
	if isDate(start) && isDate(end) {
		allDay = true
	} else {
		s, err := time.Parse(time.RFC3339, start)
		if err != nil {
			return nil, fmt.Errorf("calendar: draft start must be RFC3339 or YYYY-MM-DD")
		}
		e, err := time.Parse(time.RFC3339, end)
		if err != nil {
			return nil, fmt.Errorf("calendar: draft end must be RFC3339 or YYYY-MM-DD")
		}
		if !e.After(s) {
			return nil, fmt.Errorf("calendar: draft end must be after start")
		}
	}
	if len(description) > MaxDescription {
		description = description[:MaxDescription] + "…"
	}
	return &DraftEvent{
		Summary:     strings.TrimSpace(summary),
		Description: description,
		Location:    strings.TrimSpace(location),
		Start:       start,
		End:         end,
		AllDay:      allDay,
		Attendees:   cleanAttendees(attendees),
	}, nil
}

func isDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

func cleanAttendees(in []string) []string {
	var out []string
	for _, a := range in {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, a)
		}
	}
	return out
}

func convert(e *calendar.Event) Event {
	ev := Event{
		ID:       e.Id,
		Summary:  e.Summary,
		Location: e.Location,
		Link:     e.HtmlLink,
	}
	if d := e.Description; len(d) > MaxDescription {
		ev.Description = d[:MaxDescription] + "…"
		ev.Truncated = true
	} else {
		ev.Description = d
	}
	if e.Start != nil {
		if e.Start.Date != "" {
			ev.Start, ev.AllDay = e.Start.Date, true
		} else {
			ev.Start = e.Start.DateTime
		}
	}
	if e.End != nil {
		if e.End.Date != "" {
			ev.End = e.End.Date
		} else {
			ev.End = e.End.DateTime
		}
	}
	for _, a := range e.Attendees {
		if a != nil && strings.TrimSpace(a.Email) != "" {
			ev.Attendees = append(ev.Attendees, strings.TrimSpace(a.Email))
		}
	}
	return ev
}

func boundLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}
