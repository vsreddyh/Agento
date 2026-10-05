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
		// Cap what goes into the message. `s` is whatever the caller sent, so a
		// junk 1 MB id becomes a 1 MB error string — reflected straight back out
		// through the MCP result and into the agent's context, where it costs more
		// than the entire rest of the reply. An ObjectID is 24 hex chars, so 80
		// shows any plausible near-miss without echoing a payload.
		return primitive.NilObjectID, fail("bad id '%s'", trunc(s, 80))
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
	// A negative n would make the loop below index before the string and panic. Every
	// caller passes a positive constant today, which is exactly why a guard belongs
	// here rather than at the call sites: the next `trunc(x, computedLimit)` is the
	// one that panics, and it will be in a code path with a user in it.
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

// cleanTags lowercases, trims, caps each tag's length and the number of tags. Like
// trunc, it is a silent helper — so callers report what it dropped via truncated.
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

// tagsDropped reports whether cleanTags had to cut or discard anything, so a
// caller can say so instead of silently storing 19 of the 25 tags it was given.
//
// Blank tags are NOT counted as dropped: cleaning `" "` away is sanitation, and
// reporting it as truncation would train the caller to ignore the flag. Only the
// two lossy caps count — a tag longer than maxTagLen, and tags past maxTags.
func tagsDropped(tags []string) bool {
	kept := 0
	for _, t := range tags {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		if len(t) > maxTagLen {
			return true
		}
		kept++
		if kept > maxTags {
			return true
		}
	}
	return false
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
	out := docOut(doc)
	// A cap that discards input must say so. `trunc` is silent by design — it is a
	// helper, not a policy — but a 25 KB paste stored as 20 KB and answered with
	// ok:true is data loss reported as success. The note is still written (refusing
	// would be worse: the user's list would be lost entirely for being too long);
	// the response just carries the sizes so the caller can tell the user what
	// happened instead of believing the whole thing landed.
	out["truncated"] = map[string]any{
		"title": truncated(title, maxTitle),
		"body":  truncated(body, maxBody),
		"tags":  tagsDropped(tags),
	}
	return out, nil
}

// truncated reports whether trunc(s, n) would have dropped bytes.
func truncated(s string, n int) bool { return len(s) > n }

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
	// Tags are searched too, not just title and body. Tags are lowercased on write
	// (cleanTags), so the query is lowercased to match — `search_notes("Shopping")`
	// must find a note tagged `shopping`. Without this clause the tag was a filter
	// only reachable through list_notes(tag:), which made the tag vocabulary
	// invisible to the tool a user reaches for when they remember a word but not
	// which note holds it.
	//
	// `tags` is an array field, so a regex against it matches when ANY element
	// matches — no $elemMatch needed, and a plain equality would only ever hit an
	// exact tag.
	cur, err := s.notes.Find(ctx, bson.M{"$or": []bson.M{
		{"title": re},
		{"body": re},
		{"tags": bson.M{"$regex": regexp.QuoteMeta(strings.ToLower(q)), "$options": "i"}},
	}},
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
	upd := bson.M{}
	// What each cap dropped, collected HERE while the branch still holds the value it
	// actually stored — not in a second pass that re-derives it. The two-pass version
	// re-asserted the patch's dynamic type, so a `[]any` tags patch cleaned correctly
	// and reported nothing, and it measured the untrimmed title while the store wrote
	// the trimmed one. Any rule duplicated in two places is a rule that will drift;
	// one place means the flag cannot disagree with what was written.
	dropped := map[string]any{}
	if v, ok := patch["title"].(string); ok {
		t := strings.TrimSpace(v)
		if t == "" {
			return nil, fail("title cannot be empty")
		}
		upd["title"] = trunc(t, maxTitle)
		dropped["title"] = truncated(t, maxTitle)
	}
	if v, ok := patch["body"].(string); ok {
		upd["body"] = trunc(v, maxBody)
		dropped["body"] = truncated(v, maxBody)
	}
	if v, ok := patch["tags"]; ok && v != nil {
		var tags []string
		switch t := v.(type) {
		case []string:
			tags = t
		case []any:
			for _, x := range t {
				tags = append(tags, fmt.Sprint(x))
			}
		default:
			return nil, fail("tags must be a list of strings")
		}
		upd["tags"] = cleanTags(tags)
		dropped["tags"] = tagsDropped(tags)
	}
	if v, ok := patch["pinned"].(bool); ok {
		upd["pinned"] = v
	}
	// Nothing to change means nothing to write. `updatedAt` is added here, AFTER the
	// patch is assembled, precisely so that a patch with no recognised field issues no
	// UPDATE at all: `updatedAt` is what sorts a note to the top of every list, so a
	// no-op edit that bumped it would reorder the user's notes and imply a change that
	// never happened. The MCP layer can legitimately send an empty patch — an agent
	// that decided it had nothing to say should leave no trace.
	if len(upd) == 0 {
		return raw, nil
	}
	upd["updatedAt"] = primitive.NewDateTimeFromTime(time.Now().UTC())
	if _, err := s.notes.UpdateOne(ctx, bson.M{"_id": o}, bson.M{"$set": upd}); err != nil {
		return nil, err
	}
	out, err := s.GetNote(ctx, id)
	if err != nil || out == nil {
		return out, err
	}
	// Same honesty as AddNote, and computed from the same branch that did the write,
	// so a cap can never be reported inconsistently with what was stored.
	if len(dropped) > 0 {
		out["truncated"] = dropped
	}
	return out, nil
}

// DeleteNote reports whether a note was actually deleted. A missing id is NOT an
// error here — deletion is idempotent by nature, and "it was already gone" is a
// result the caller can act on. The MCP layer turns `false` into `ok:false` with an
// explanatory message, so every tool in this server reports a missing note the same
// way instead of one of them answering `ok:true`.
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
	// Tags are always []string on the way out, whichever path produced the doc.
	// A freshly inserted doc holds the Go []string that cleanTags returned; a doc
	// read back from Mongo decodes the BSON array as bson.A. The two are identical
	// over JSON but not identical in Go, so `note["tags"].([]string)` succeeds on
	// the create response and PANICS on the get — a type assertion that passes in a
	// test written against the create path and fails in production on the read path.
	// Normalising here means every caller can assert once.
	if raw, ok := out["tags"]; ok {
		out["tags"] = toStringSlice(raw)
	}
	if body, ok := out["body"].(string); ok {
		out["preview"] = preview(body)
	}
	return out
}

// toStringSlice renders a BSON array or an existing []string as []string, never
// nil, so `out["tags"].([]string)` always succeeds.
func toStringSlice(v any) []string {
	switch t := v.(type) {
	case []string:
		if t == nil {
			return []string{}
		}
		return t
	case bson.A:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, fmt.Sprint(e))
		}
		return out
	default:
		return []string{}
	}
}

// headingRe matches an ATX heading and only a heading: 1-6 '#' followed by
// whitespace or end of line. A bare `strings.HasPrefix(line, "#")` is broader than
// that and eats content: "#milk" and "#1 milk" are grocery items, not headings, and
// skipping them means a note whose first line is a tag previews as whatever comes
// after it. Markdown requires the space.
var headingRe = regexp.MustCompile(`^#{1,6}(\s|$)`)

// preview is the first line of the body that says something: blanks and ATX
// headings ("# Groceries") are skipped, because `title` already carries the heading
// and repeating it wastes the one field a summary has. A grocery list previews as
// its first item — "- milk" — which is what lets the agent tell two similar notes
// apart without fetching either body.
//
// Returns "" for a body with no such line (empty, whitespace, or headings only),
// which is the honest answer: there is nothing to preview.
func preview(body string) string {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || headingRe.MatchString(line) {
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
