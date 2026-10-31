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
	// MaxSummary caps draft summaries and locations. Outbound cuts refuse
	// rather than truncate (same contract as gmail drafts): silently
	// shortening text the user will confirm is data loss.
	MaxSummary  = 200
	MaxLocation = 500
	// DefaultWindow bounds an unfiltered list: upcoming means the next 30
	// days, not the whole calendar history.
	DefaultWindow = 30 * 24 * time.Hour
)

// Client calls the Calendar API as the connected grant. The grant resolves
// per call, not at construction: a fresh deploy with no grant yet must
// still boot (the entrypoint probes every server), and every tool answers
// reconnect guidance until the user connects. No grant (or a dead one) is
// a reconnect error, never a secrets prompt.
type Client struct {
	store    connectors.Store
	cfg      *connectors.OAuthConfig
	endpoint string
	// logf carries provider-side refresh failure detail to container
	// logs (nil = silent). Test-only callers leave it nil.
	logf func(string, ...any)
	// static bypasses the vault: test-only, set by NewClientWithToken.
	static *calendar.Service
}

// NewClient binds store + config + endpoint. It needs no grant and no
// OAuth client pair: Mongo is the only boot requirement, so construction
// fails only on programmer error (nil store), never on a disconnected user
// and never on a deploy without Google OAuth — a nil cfg boots fine and
// every Google-touching call answers "not configured" per call (see
// connectors.GrantTokenSource) until the deploy is configured.
func NewClient(store connectors.Store, cfg *connectors.OAuthConfig, endpoint string, logf func(string, ...any)) (*Client, error) {
	if store == nil {
		return nil, fmt.Errorf("calendar: client needs a store")
	}
	return &Client{store: store, cfg: cfg, endpoint: endpoint, logf: logf}, nil
}

// NewClientWithToken builds a Client around an explicit token source.
// Test-only: production always resolves the vault grant per call.
func NewClientWithToken(ctx context.Context, src oauth2.TokenSource, endpoint string) (*Client, error) {
	opts := []option.ClientOption{option.WithTokenSource(src)}
	if endpoint != "" {
		opts = append(opts, option.WithEndpoint(endpoint))
	}
	svc, err := calendar.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{static: svc}, nil
}

// service resolves the Calendar API client for one call: the vault grant
// (refreshing and persisting on expiry), or the static test service.
// Grant errors surface here — per tool call, never at boot.
// callCtx bounds one Google API call: Pi issues no deadline of its own,
// so an unbounded call would hang the tool (and the agent turn) on a hung
// upstream. 20s covers a slow API without letting one call stall a turn.
func callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 20*time.Second)
}

func (c *Client) service(ctx context.Context) (*calendar.Service, error) {
	if c.static != nil {
		return c.static, nil
	}
	httpClient, err := connectors.GrantClient(ctx, c.store, c.cfg, connectors.Calendar, c.logf)
	if err != nil {
		return nil, err
	}
	opts := []option.ClientOption{option.WithHTTPClient(httpClient)}
	if c.endpoint != "" {
		opts = append(opts, option.WithEndpoint(c.endpoint))
	}
	return calendar.NewService(ctx, opts...)
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
	// Cheap local checks before the grant resolves: a bad window is a
	// caller error, and answering it before touching the store keeps
	// error precedence symmetric with the gmail client (validate
	// first, authenticate second) instead of masking it as reconnect.
	// Calendar id normalisation lives here for the same reason: no
	// store round-trip for a trimmable string.
	calendarID = strings.TrimSpace(calendarID)
	if calendarID == "" {
		calendarID = "primary"
	}
	now := time.Now()
	if from.IsZero() {
		from = now
	}
	if to.IsZero() {
		// Anchored to from (already defaulted to now), not to wall-clock
		// now: an explicit from=2027-06-01 with default to must yield a
		// window after X, not now+30d < from and a 400 on a reasonable
		// query.
		to = from.Add(DefaultWindow)
	}
	if !to.After(from) {
		return nil, fmt.Errorf("calendar: list needs an end after its start")
	}
	limit = boundLimit(limit)
	ctx, cancel := callCtx(ctx)
	defer cancel()
	svc, err := c.service(ctx)
	if err != nil {
		return nil, err
	}
	res, err := svc.Events.List(calendarID).
		TimeMin(from.Format(time.RFC3339)).
		TimeMax(to.Format(time.RFC3339)).
		MaxResults(int64(limit)).
		SingleEvents(true).
		OrderBy("startTime").
		Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("calendar: list events: %w", err)
	}
	if res == nil {
		return nil, fmt.Errorf("calendar: list events: empty response")
	}
	out := make([]Event, 0, len(res.Items))
	for _, e := range res.Items {
		// No second fetch per item (unlike gmail rows), so a nil
		// element is a malformed upstream response, not a deletion:
		// fail closed instead of skipping.
		if e == nil {
			return nil, fmt.Errorf("calendar: list events: upstream answered a missing event")
		}
		out = append(out, convert(e))
	}
	return out, nil
}

// Get returns one event by id.
func (c *Client) Get(ctx context.Context, calendarID, id string) (*Event, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("calendar: get needs an event id")
	}
	calendarID = strings.TrimSpace(calendarID)
	if calendarID == "" {
		calendarID = "primary"
	}
	ctx, cancel := callCtx(ctx)
	defer cancel()
	svc, err := c.service(ctx)
	if err != nil {
		return nil, err
	}
	e, err := svc.Events.Get(calendarID, strings.TrimSpace(id)).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("calendar: get event: %w", err)
	}
	if e == nil {
		return nil, fmt.Errorf("calendar: get event: empty response")
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
	// Truncated reports an over-cap description cut.
	Truncated bool `json:"truncated,omitempty"`
}

// Draft validates event fields and returns the draft WITHOUT calling
// Google: a readonly grant cannot write, and this tool must work on
// exactly the grant the connect flow mints. Start/end accept RFC3339
// date-times or YYYY-MM-DD dates (both ends must match in kind); end must
// be after start in both kinds.
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
		// ISO dates order lexicographically, so the string compare is
		// the date compare — no parse needed for the check itself.
		// All-day ends are exclusive in Google's model, so equal ends
		// are rejected the same way as reversed ones.
		if end <= start {
			return nil, fmt.Errorf("calendar: draft end must be after start")
		}
	} else if isDate(start) || isDate(end) {
		return nil, fmt.Errorf("calendar: draft start and end must match in kind (both dates or both RFC3339)")
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
	truncated := false
	description = strings.TrimSpace(description)
	summary = strings.TrimSpace(summary)
	location = strings.TrimSpace(location)
	if t, cut := truncateRunes(description, MaxDescription); cut {
		description, truncated = t, true
	}
	if n := len([]rune(summary)); n > MaxSummary {
		return nil, fmt.Errorf("calendar: draft summary too long (%d runes, max %d)", n, MaxSummary)
	}
	if n := len([]rune(location)); n > MaxLocation {
		return nil, fmt.Errorf("calendar: draft location too long (%d runes, max %d)", n, MaxLocation)
	}
	attendees = cleanAttendees(attendees)
	for _, a := range attendees {
		// Same shape contract as gmail To (single bare ASCII address,
		// no display names, no separators, no line breaks): the create
		// path validates for real, but an obvious typo, an injected
		// header, or a multi-address string fails here instead of
		// after user confirm.
		if !strings.Contains(a, "@") || !isASCII(a) {
			return nil, fmt.Errorf("calendar: draft attendee %q must be a single ASCII email address", a)
		}
		if strings.ContainsAny(a, " \t<>,;\r\n") {
			return nil, fmt.Errorf("calendar: draft takes one bare address per attendee, not %q", a)
		}
	}
	return &DraftEvent{
		Summary:     summary,
		Description: description,
		Location:    location,
		Start:       start,
		End:         end,
		AllDay:      allDay,
		Attendees:   attendees,
		Truncated:   truncated,
	}, nil
}

func isDate(s string) bool {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return false
	}
	// time.Parse normalizes day overflow (Feb 30 becomes Mar 2), so a
	// round-trip check is the existence check: Google rejects
	// non-existent dates at creation time, and the draft should fail
	// here instead of after the user confirmed it.
	return t.Format("2006-01-02") == s
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

func cleanAttendees(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range in {
		// Compare folded: Gmail delivery is case-insensitive, so
		// "A@x.com" and "a@x.com" are the same invitee. Keep the
		// first spelling for display.
		if a = strings.TrimSpace(a); a == "" || seen[strings.ToLower(a)] {
			continue
		}
		seen[strings.ToLower(a)] = true
		out = append(out, a)
	}
	return out
}

func convert(e *calendar.Event) Event {
	// Reads are verbatim by design (see plainBody's documented is_html
	// contract on the gmail side): summary/location/description pass
	// through uncapped, while Draft caps + truncates + reports. A stored
	// event with a 2KB summary surfaces unbounded here, then fails if
	// round-tripped into a draft — the read and write sides differ on
	// purpose (never rewrite stored mail; never silently cut a draft
	// the user will confirm). Same verbatim rule drops e.Status: a
	// cancelled event fetched by id reads as a normal event (lists
	// filter cancelled by default, so only direct Get hits this).
	// Attendees ride along verbatim too (no trim, no case-fold
	// dedupe) — a Get → Draft round-trip can change length by
	// whitespace, case, or dupes alone. Same intentional asymmetry:
	// never rewrite stored mail; drafts normalise before the user
	// confirms.
	//
	// Nil-guarded like gmail's readMessage: callers check, but the
	// converter itself must not panic on a malformed upstream row.
	if e == nil {
		return Event{}
	}
	ev := Event{
		ID:       e.Id,
		Summary:  e.Summary,
		Location: e.Location,
		Link:     e.HtmlLink,
	}
	// Reads are verbatim by design (never rewrite stored mail); drafts
	// are normalised (see Draft). A whitespace-only round-trip Get →
	// Draft can therefore flip Truncated/length — that is the read and
	// write contracts differing on purpose, not drift.
	if d := e.Description; d != "" {
		if t, cut := truncateRunes(d, MaxDescription); cut {
			ev.Description, ev.Truncated = t, true
		} else {
			ev.Description = d
		}
	}
	// AllDay needs both ends as dates: upstream always sends matching
	// kinds, but a mixed response (date start, timed end) must not
	// surface as all-day with a timed end — Draft rejects mixed kinds,
	// so the read shape must not invent one. A missing end with a date
	// start reads as all-day with no End (upstream always pairs them,
	// so this only fires on malformed rows — surfacing the gap beats
	// inventing an end date); a timed end with a date start surfaces
	// the date verbatim with AllDay false.
	if e.Start != nil {
		if e.Start.Date != "" && (e.End == nil || e.End.Date != "") {
			ev.Start = e.Start.Date
			ev.AllDay = true
		} else if e.Start.Date != "" {
			// Mixed kinds (date start, timed end): malformed upstream.
			// Surface the present date verbatim, AllDay false — never
			// invent all-day, matching Draft's mixed-kind refusal.
			ev.Start = e.Start.Date
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

// truncateRunes cuts s to max runes (not bytes — a byte cut can split a
// multi-byte rune mid-sequence), reporting whether it cut. The cut keeps
// max content runes with the marker appended past them, so the result is
// max+1 runes and the cut is visible.
func truncateRunes(s string, max int) (string, bool) {
	runes := []rune(s)
	if len(runes) <= max {
		return s, false
	}
	return string(runes[:max]) + "…", true
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
