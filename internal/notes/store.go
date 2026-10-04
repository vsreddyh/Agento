// Package notes implements MongoDB storage for the notes MCP.
//
// Collection (permanent — never pruned): notes.
//
// Scope: one shared collection, like money / health / cookbook — a grocery list
// belongs to the user, not to whichever profile happened to be asked. There is
// deliberately no `profile` field: adding one later is a migration-free additive
// change, and carrying it now would push the agent to ask a question ("which
// profile?") that has no meaningful answer for a shopping list.
//
// Body is markdown, not a list of item rows. The consumer is a human reading a
// list on a phone and an agent editing it from speech ("I got milk" -> delete
// the line), so lines are the unit of work and a row-per-item schema would buy
// per-item state nobody reads, at the cost of a second representation to keep
// consistent.
package notes

import (
	"context"
	"errors"
	"fmt"
	"regexp"
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
	// maxBody caps a note. A grocery list or a paragraph of prose is far under this;
	// the cap exists so one paste cannot blow up a context window.
	maxBody = 20000
	// maxTitle is a display label, not prose.
	maxTitle = 120
	// maxTags is a filter vocabulary, not a labelling system.
	maxTags = 20
	// maxTagLen keeps a tag one word-ish.
	maxTagLen = 40
	// listCap is the ceiling for list_notes, which returns summaries. Matches cookbook.
	listCap = 200
	// searchCap is much lower than listCap on purpose: search returns FULL notes, so
	// the ceiling is 50 bodies rather than 50 summaries. At maxBody the worst case is
	// 1 MB of text in one tool result, which defeats the same context-window care
	// ListNotes is built around. A search that matches more than this wants
	// list_notes (summaries) plus a narrower query, not more bodies.
	searchCap = 50
)

// StoreError is the domain error.
type StoreError struct{ Msg string }

func (e *StoreError) Error() string { return e.Msg }

func fail(format string, args ...any) *StoreError {
	return &StoreError{Msg: fmt.Sprintf(format, args...)}
}

func oid(s string) (primitive.ObjectID, error) {
	o, err := primitive.ObjectIDFromHex(strings.TrimSpace(s))
	if err != nil {
		return primitive.NilObjectID, fail("bad id '%s'", s)
	}
	return o, nil
}

// trunc caps a string at n BYTES without splitting a rune. `s[:n]` on a multi-byte
// character produces invalid UTF-8, and a note is exactly the place a user writes an
// emoji or a non-Latin script ("🥑", "café", "दूध") — the note would be silently
// corrupted at the cap, and the corruption is invisible until something downstream
// tries to render it. Cut on a rune boundary instead, backing off up to utf8.UTFMax-1
// bytes.
func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func cleanTags(tags []string) []string {
	out := make([]string, 0, len(tags))
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		out = append(out, trunc(t, maxTagLen))
		if len(out) >= maxTags {
			break
		}
	}
	return out
}

// Store binds the notes collection + indexes on first use.
type Store struct {
	notes *mongoDrv.Collection
}

func New() (*Store, error) {
	d, err := mongo.DB()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := &Store{notes: d.Collection("notes")}
	// Title is NOT unique, unlike cookbook recipes: two notes called "list" from
	// different months is normal, and forcing the agent to invent unique titles
	// would put a naming burden in front of every grocery trip.
	if _, err := s.notes.Indexes().CreateMany(ctx, []mongoDrv.IndexModel{
		{Keys: bson.D{{Key: "pinned", Value: -1}, {Key: "updatedAt", Value: -1}}},
		{Keys: bson.D{{Key: "tags", Value: 1}}},
	}); err != nil {
		return nil, err
	}
	return s, nil
}

// FromEnv builds a Store from env (kept for symmetry with the other stores).
func FromEnv() (*Store, error) { return New() }

func (s *Store) AddNote(ctx context.Context, title, body string, tags []string, pinned bool) (bson.M, error) {
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, fail("title is required")
	}
	now := primitive.NewDateTimeFromTime(time.Now().UTC())
	doc := bson.M{
		"title":     trunc(title, maxTitle),
		"body":      trunc(body, maxBody),
		"tags":      cleanTags(tags),
		"pinned":    pinned,
		"createdAt": now,
		"updatedAt": now,
	}
	res, err := s.notes.InsertOne(ctx, doc)
	if err != nil {
		return nil, err
	}
	doc["_id"] = res.InsertedID
	return docOut(doc), nil
}

// GetNote resolves by id only. Unlike cookbook, there is no title lookup: titles
// are not unique, so resolving one would silently pick a winner among notes the
// user considers distinct. get_note takes the id that list_notes returned.
//
// ErrNoDocuments is the ONLY outcome reported as "not found" (nil, nil). Every
// other FindOne error is propagated, because a flat (nil, nil) here turns a
// timeout, a dropped connection or an auth failure into "unknown note '<id>'" —
// the agent then tells the user their list does not exist when MongoDB is simply
// down. A missing note is a fact; an unreachable database is a fault.
func (s *Store) GetNote(ctx context.Context, id string) (bson.M, error) {
	o, err := oid(id)
	if err != nil {
		return nil, err
	}
	var n bson.M
	if err := s.notes.FindOne(ctx, bson.M{"_id": o}).Decode(&n); err != nil {
		if errors.Is(err, mongoDrv.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	return docOut(n), nil
}

// ListNotes returns summaries, not bodies: a filter query over 200 notes must not
// return 200 grocery lists into the context window. The first non-empty body line
// is included as `preview` so the agent can pick the right note to open.
//
// The body IS fetched, then previewed and dropped — `SetProjection(bson.M{"body":0})`
// would be cheaper but also strips the text `preview` is derived from, so the
// projection version of this function returned summaries with no preview at all and
// the agent had nothing to choose between. One extra field off the wire per row buys
// back the ability to identify a note by its first item, which is the entire job of
// a summary. summaries() does the stripping, so the rule lives in one place.
func (s *Store) ListNotes(ctx context.Context, tag string, pinnedOnly bool, limit int) ([]bson.M, error) {
	filt := bson.M{}
	if t := strings.TrimSpace(tag); t != "" {
		filt["tags"] = strings.ToLower(t)
	}
	if pinnedOnly {
		filt["pinned"] = true
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > listCap {
		limit = listCap
	}
	cur, err := s.notes.Find(ctx, filt,
		options.Find().
			SetSort(bson.D{{Key: "pinned", Value: -1}, {Key: "updatedAt", Value: -1}}).
			SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	rows, err := allOut(ctx, cur)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		summarize(r)
	}
	return rows, nil
}

// summarize turns a full note into a summary: body replaced by the preview of it.
// Applied after docOut so `preview` is computed from the text while it is still
// present, and idempotent, so a caller that already summarized may call it again.
func summarize(n bson.M) {
	body, _ := n["body"].(string)
	n["preview"] = preview(body)
	delete(n, "body")
}

// SearchNotes matches body text too, which is the point of a grocery list: the
// user says "milk" and does not remember which note holds it. Returns full docs,
// because a search hit whose text you cannot see is useless.
func (s *Store) SearchNotes(ctx context.Context, q string, limit int) ([]bson.M, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, fail("search text is required")
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > searchCap {
		limit = searchCap
	}
	re := bson.M{"$regex": regexp.QuoteMeta(q), "$options": "i"}
	cur, err := s.notes.Find(ctx, bson.M{"$or": []bson.M{{"title": re}, {"body": re}}},
		options.Find().
			SetSort(bson.D{{Key: "pinned", Value: -1}, {Key: "updatedAt", Value: -1}}).
			SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	return allOut(ctx, cur)
}

// UpdateNote is patch-shaped: absent keys are left alone, and the tool description
// says so. A whole-record resend is how an edit to one line of a grocery list
// silently drops the lines the agent did not include.
func (s *Store) UpdateNote(ctx context.Context, id string, patch map[string]any) (bson.M, error) {
	raw, err := s.GetNote(ctx, id)
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
	upd := bson.M{"updatedAt": primitive.NewDateTimeFromTime(time.Now().UTC())}
	if v, ok := patch["title"].(string); ok {
		t := strings.TrimSpace(v)
		if t == "" {
			return nil, fail("title cannot be empty")
		}
		upd["title"] = trunc(t, maxTitle)
	}
	if v, ok := patch["body"].(string); ok {
		upd["body"] = trunc(v, maxBody)
	}
	if v, ok := patch["tags"]; ok && v != nil {
		var raw []string
		switch t := v.(type) {
		case []string:
			raw = t
		case []any:
			for _, x := range t {
				raw = append(raw, fmt.Sprint(x))
			}
		default:
			return nil, fail("tags must be a list of strings")
		}
		upd["tags"] = cleanTags(raw)
	}
	if v, ok := patch["pinned"].(bool); ok {
		upd["pinned"] = v
	}
	if _, err := s.notes.UpdateOne(ctx, bson.M{"_id": o}, bson.M{"$set": upd}); err != nil {
		return nil, err
	}
	return s.GetNote(ctx, id)
}

func (s *Store) DeleteNote(ctx context.Context, id string) (bool, error) {
	o, err := oid(id)
	if err != nil {
		return false, err
	}
	res, err := s.notes.DeleteOne(ctx, bson.M{"_id": o})
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
	for _, k := range []string{"createdAt", "updatedAt"} {
		if v, ok := out[k]; ok && v != nil {
			out[k] = fmt.Sprint(v)
		}
	}
	if body, ok := out["body"].(string); ok {
		out["preview"] = preview(body)
	}
	return out
}

// preview is the first non-blank, non-heading body line, capped. A grocery list
// previews as its first item, which is the whole point of previewing.
func preview(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		return trunc(line, 80)
	}
	return ""
}

func allOut(ctx context.Context, cur *mongoDrv.Cursor) ([]bson.M, error) {
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make([]bson.M, 0, len(rows))
	for _, r := range rows {
		out = append(out, docOut(r))
	}
	return out, nil
}
