// Package reports implements MongoDB storage for the ∃-Mail inbox (#365).
//
// Collection (7-day conditional retention — NOT permanent, NOT cookbook): reports.
//
// Scope: a read-only inbox of reports filed by agents. The user just reads them —
// not email: no compose, no reply, no send, no external addresses. Writers file
// via the reports MCP server (create_report); readers are the health-api routes
// (the app's ∃-Mail screen) and the retention job. The collection is shared, like
// money / health / cookbook: a report belongs to the user, not to whichever
// profile happened to be running when the agent filed it, so there is
// deliberately no `profile` field.
//
// Retention rule (like money/health, unlike cookbook-permanent): a report is
// deleted when age(createdAt) > 7d AND read == true AND starred == false.
// Unread stays regardless of age; starred stays regardless of age. See
// cmd/retention.
package reports

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"agento/internal/mongo"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	// maxTitle is a display label, not prose.
	maxTitle = 200
	// maxBody caps a report. Agent reports with math run longer than a grocery
	// list; the cap exists so one paste cannot blow up a context window.
	maxBody = 50000
	// maxSource caps the skill/agent id that filed the report.
	maxSource = 80
	// listCap is the ceiling for lists, which return summaries.
	listCap = 200
)

// StoreError is the domain error.
type StoreError struct{ Msg string }

func (e *StoreError) Error() string { return e.Msg }

func fail(format string, args ...any) *StoreError {
	return &StoreError{Msg: fmt.Sprintf(format, args...)}
}

// ShortID renders a caller-supplied id for an error message: capped, and cut on a
// rune boundary so a non-ASCII id cannot put invalid UTF-8 into the JSON result.
func ShortID(s string) string {
	const cap = 80
	if len(s) <= cap {
		return s
	}
	return trunc(s, cap) + "…"
}

func oid(s string) (primitive.ObjectID, error) {
	o, err := primitive.ObjectIDFromHex(strings.TrimSpace(s))
	if err != nil {
		return primitive.NilObjectID, fail("bad id '%s'", ShortID(s))
	}
	return o, nil
}

// trunc caps a string at n BYTES without splitting a rune.
func trunc(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// truncated reports whether trunc(s, n) would have dropped bytes.
func truncated(s string, n int) bool { return len(s) > n }

// Store binds the reports collection + indexes on first use.
type Store struct {
	reports *mongoDrv.Collection
}

func New() (*Store, error) {
	d, err := mongo.DB()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := &Store{reports: d.Collection("reports")}
	// Newest-first is the only list order (∃-Mail shows newest on top), so the
	// sort key gets the index. Titles are not unique and reports are addressed
	// by id only.
	if _, err := s.reports.Indexes().CreateMany(ctx, []mongoDrv.IndexModel{
		{Keys: bson.D{{Key: "createdAt", Value: -1}}},
	}); err != nil {
		return nil, err
	}
	return s, nil
}

// FromEnv builds a Store from env (kept for symmetry with the other stores).
func FromEnv() (*Store, error) { return New() }

// CreateReport files one report. read and starred always start false: the
// writer files, the reader's opening marks read, the retention rule eats
// read+unstarred after 7 days.
func (s *Store) CreateReport(ctx context.Context, title, bodyMD, source string) (bson.M, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fail("title is required")
	}
	now := primitive.NewDateTimeFromTime(time.Now().UTC())
	doc := bson.M{
		"title":     trunc(title, maxTitle),
		"body_md":   trunc(bodyMD, maxBody),
		"source":    trunc(strings.TrimSpace(source), maxSource),
		"read":      false,
		"starred":   false,
		"createdAt": now,
	}
	res, err := s.reports.InsertOne(ctx, doc)
	if err != nil {
		return nil, err
	}
	doc["_id"] = res.InsertedID
	out := docOut(doc)
	out["truncated"] = map[string]any{
		"title":   truncated(title, maxTitle),
		"body_md": truncated(bodyMD, maxBody),
		"source":  truncated(strings.TrimSpace(source), maxSource),
	}
	return out, nil
}

// GetReport resolves by id only. ErrNoDocuments is the ONLY outcome reported as
// "not found" (nil, nil); every other error propagates, so an unreachable
// database never reads as "unknown report".
func (s *Store) GetReport(ctx context.Context, id string) (bson.M, error) {
	o, err := oid(id)
	if err != nil {
		return nil, err
	}
	var r bson.M
	if err := s.reports.FindOne(ctx, bson.M{"_id": o}).Decode(&r); err != nil {
		if errors.Is(err, mongoDrv.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return docOut(r), nil
}

// ListReports returns newest-first summaries, not bodies: a filter query over
// 200 reports must not return 200 bodies into the context window. The first
// non-empty body line is included as `preview` so the agent can pick the right
// report to open.
func (s *Store) ListReports(ctx context.Context, limit int) ([]bson.M, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > listCap {
		limit = listCap
	}
	cur, err := s.reports.Find(ctx, bson.M{},
		options.Find().
			SetSort(bson.D{{Key: "createdAt", Value: -1}}).
			SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]bson.M, 0, len(rows))
	for _, r := range rows {
		d := docOut(r)
		summarize(d)
		out = append(out, d)
	}
	return out, nil
}

// summarize turns a full report into a summary: body_md replaced by its
// preview. Applied after docOut so `preview` is computed from the text while it
// is still present, and idempotent.
func summarize(r bson.M) {
	body, _ := r["body_md"].(string)
	r["preview"] = preview(body)
	delete(r, "body_md")
}

// UpdateReport applies the reader's verbs: opening marks read, the star
// toggles. Only "read" and "starred" are patchable — a report's content is
// immutable after filing (v1 has no re-file/edit path), so unknown keys are
// refused loudly rather than ignored.
func (s *Store) UpdateReport(ctx context.Context, id string, patch map[string]any) (bson.M, error) {
	raw, err := s.GetReport(ctx, id)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	o, err := oid(id)
	if err != nil {
		return nil, err
	}
	upd := bson.M{}
	for _, k := range []string{"read", "starred"} {
		if v, ok := patch[k]; ok && v != nil {
			b, ok := v.(bool)
			if !ok {
				return nil, fail("%s must be a boolean", k)
			}
			upd[k] = b
		}
	}
	for k := range patch {
		if k != "read" && k != "starred" && patch[k] != nil {
			return nil, fail("unknown field '%s' (only read and starred are patchable)", k)
		}
	}
	if len(upd) == 0 {
		return raw, nil
	}
	if _, err := s.reports.UpdateOne(ctx, bson.M{"_id": o}, bson.M{"$set": upd}); err != nil {
		return nil, err
	}
	return s.GetReport(ctx, id)
}

// DeleteReport hard-deletes one report and reports whether it existed. Missing
// is NOT an error — deletion is idempotent, and the caller reports "nothing
// was deleted" the same way get does.
func (s *Store) DeleteReport(ctx context.Context, id string) (bool, error) {
	o, err := oid(id)
	if err != nil {
		return false, err
	}
	res, err := s.reports.DeleteOne(ctx, bson.M{"_id": o})
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}

func docOut(d bson.M) bson.M {
	out := bson.M{}
	for k, v := range d {
		out[k] = v
	}
	out["_id"] = mongo.IDString(d["_id"])
	// Canonical wire id for the app (#433). `_id` stays for the MCP tools and
	// older readers; the phone reads `id` only, like every other list.
	out["id"] = out["_id"]
	if v, ok := out["createdAt"]; ok && v != nil {
		out["createdAt"] = fmt.Sprint(v)
	}
	// read/starred default false for docs written before the flags existed.
	for _, k := range []string{"read", "starred"} {
		if b, ok := out[k].(bool); !ok || !b {
			if _, ok := out[k].(bool); !ok {
				out[k] = false
			}
		}
	}
	return out
}

// preview is the first body line that says something: blanks and ATX headings
// are skipped, because `title` already carries the heading.
func preview(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || isHeading(line) {
			continue
		}
		return trunc(line, 160)
	}
	return ""
}

func isHeading(line string) bool {
	for i := 0; i < len(line) && i < 6; i++ {
		if line[i] != '#' {
			return false
		}
		if i+1 == len(line) || line[i+1] == ' ' || line[i+1] == '\t' {
			return true
		}
	}
	return false
}
