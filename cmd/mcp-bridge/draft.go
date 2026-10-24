package main

import (
	"fmt"
	"strings"
	"time"
)

// create_event_draft is bridge-local: the official Calendar server's own
// create path is filtered (draft-only), so the bridge validates and returns
// the draft WITHOUT calling Google. The agent presents it to the user;
// actual creation waits for the approval-gate flow that covers writes.

const draftToolName = "create_event_draft"

// MaxDraftDescription caps one draft description; over-cap text is
// truncated and reported, never silently cut.
const MaxDraftDescription = 5000

func draftToolSpec() map[string]any {
	return map[string]any{
		"name":        draftToolName,
		"description": "Build a draft calendar event (validated, sent nowhere). The agent presents the draft to the user; nothing is created. Start/end are RFC3339 date-times or YYYY-MM-DD dates (both ends must match in kind); end must be after start.",
		"inputSchema": map[string]any{
			"type":     "object",
			"required": []string{"summary", "start", "end"},
			"properties": map[string]any{
				"summary":     map[string]any{"type": "string", "description": "Event title."},
				"description": map[string]any{"type": "string", "description": "Longer description. Over-cap text is truncated and reported."},
				"location":    map[string]any{"type": "string", "description": "Where it happens."},
				"start":       map[string]any{"type": "string", "description": "RFC3339 date-time or YYYY-MM-DD date."},
				"end":         map[string]any{"type": "string", "description": "RFC3339 date-time or YYYY-MM-DD date. Must be after start."},
				"attendees":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Invitees. Stored on the draft only — nothing is sent anywhere."},
			},
		},
	}
}

func callDraft(args map[string]any) (any, error) {
	str := func(key string) string {
		v, _ := args[key].(string)
		return strings.TrimSpace(v)
	}
	var attendees []string
	if raw, ok := args["attendees"].([]any); ok {
		for _, a := range raw {
			if s, _ := a.(string); strings.TrimSpace(s) != "" {
				attendees = append(attendees, strings.TrimSpace(s))
			}
		}
	}
	d, truncated, err := buildDraft(str("summary"), str("description"), str("location"), str("start"), str("end"), attendees)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"draft":     d,
		"truncated": truncated,
		"note":      "draft only — nothing was created or sent; present it to the user",
	}, nil
}

// buildDraft validates event fields and returns the draft. Start/end accept
// RFC3339 date-times or YYYY-MM-DD dates (both ends must match in kind).
func buildDraft(summary, description, location, start, end string, attendees []string) (map[string]any, bool, error) {
	if summary == "" {
		return nil, false, fmt.Errorf("calendar: draft needs a summary")
	}
	if start == "" || end == "" {
		return nil, false, fmt.Errorf("calendar: draft needs a start and an end")
	}
	allDay := false
	startDate, startErr := time.Parse("2006-01-02", start)
	endDate, endErr := time.Parse("2006-01-02", end)
	switch {
	case startErr == nil && endErr == nil:
		allDay = true
		if !endDate.After(startDate) && !endDate.Equal(startDate) {
			return nil, false, fmt.Errorf("calendar: draft end must be after start")
		}
	case startErr == nil || endErr == nil:
		return nil, false, fmt.Errorf("calendar: draft start and end must match in kind (both dates or both RFC3339)")
	default:
		s, err := time.Parse(time.RFC3339, start)
		if err != nil {
			return nil, false, fmt.Errorf("calendar: draft start must be RFC3339 or YYYY-MM-DD")
		}
		e, err := time.Parse(time.RFC3339, end)
		if err != nil {
			return nil, false, fmt.Errorf("calendar: draft end must be RFC3339 or YYYY-MM-DD")
		}
		if !e.After(s) {
			return nil, false, fmt.Errorf("calendar: draft end must be after start")
		}
	}
	truncated := false
	if len(description) > MaxDraftDescription {
		description = description[:MaxDraftDescription] + "…"
		truncated = true
	}
	return map[string]any{
		"summary": summary, "description": description,
		"location": location, "start": start, "end": end,
		"all_day": allDay, "attendees": attendees,
	}, truncated, nil
}
