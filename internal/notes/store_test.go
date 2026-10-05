package notes

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("MONGODB_URI") == "" {
		t.Skip("MONGODB_URI not set")
	}
	// Its OWN throwaway database, not the shared `miser_test` one the other suites use.
	// `go test ./...` runs packages in parallel, and internal/money and internal/tasks
	// both call db.Drop(ctx) on `miser_test` — a wholesale drop, not a scoped delete.
	// Sharing that database made this suite fail whenever it happened to run alongside
	// them: the notes collection was dropped between AddNote and DeleteNote and the
	// round trip reported "delete: false" with no error anywhere. A test that fails
	// based on which package the scheduler happened to pair it with is not a test.
	//
	// (The same hazard still exists between money/tasks and cookbook, which is their
	// bug and not this PR's — but notes does not have to inherit it.)
	t.Setenv("MONGODB_URI", os.Getenv("MONGODB_URI"))
	t.Setenv("MONGODB_DB", "miser_test_notes")
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestNoteRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := s.AddNote(ctx, "  ", "x", nil, false); err == nil {
		t.Fatal("expected empty-title error")
	}
	// Idempotent: clear leftovers from interrupted runs. Titles are NOT unique, so
	// this cleans by searching for the tag rather than by name.
	cleanup(t, s, ctx)

	// A second, unpinned, differently-tagged note. Created up front because the
	// search assertions below need a note whose ONLY match is a tag, and the
	// pinned-first ordering assertion needs a rival to outrank.
	other, err := s.AddNote(ctx, "gotest unpinned", "- soap", []string{"errands"}, false)
	if err != nil {
		t.Fatalf("add rival: %v", err)
	}
	otherID := other["_id"].(string)
	defer func() { _, _ = s.DeleteNote(ctx, otherID) }()

	n, err := s.AddNote(ctx, "gotest groceries", "- milk\n- eggs\n\n- bread", []string{"Test", " Shopping "}, true)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	id := n["_id"].(string)
	if n["preview"] != "- milk" {
		t.Errorf("preview = %v, want the first non-blank line", n["preview"])
	}
	// Tags are lowercased and trimmed, so `list_notes(tag:"shopping")` matches a note
	// tagged " Shopping ".
	if got := n["tags"].([]string); got[0] != "test" || got[1] != "shopping" {
		t.Errorf("tags = %v, want lowercased/trimmed", got)
	}

	// list_notes must NOT carry the body: 200 grocery lists must not enter the context.
	rows, err := s.ListNotes(ctx, "shopping", false, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	if _, ok := rows[0]["body"]; ok {
		t.Error("list_notes returned a body — the body is not being stripped")
	}
	// ...but it MUST carry the preview. The summary exists so the agent can tell the
	// notes apart; a summary with no preview and no body is a list of titles and
	// nothing else, which is what the projection-only version returned.
	if rows[0]["preview"] != "- milk" {
		t.Errorf("preview = %v, want the first non-blank body line", rows[0]["preview"])
	}

	// Pinned-first ordering: the pinned note is this one and the rival is not.
	rows, err = s.ListNotes(ctx, "", false, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("list all: %v %v", rows, err)
	}
	if rows[0]["_id"] != id {
		t.Errorf("first row = %v, want the pinned note first", rows[0]["_id"])
	}

	// Body search is the reason search_notes exists: the user says "milk" without
	// naming the note.
	hits, err := s.SearchNotes(ctx, "eggs", 0)
	if err != nil || len(hits) != 1 {
		t.Fatalf("search: %v %v", hits, err)
	}
	if _, ok := hits[0]["body"]; !ok {
		t.Error("search_notes must return the body it matched")
	}

	// Tags are searchable too, and case-insensitively: a word the user remembers
	// has to find the note when it is only a tag. `rival` below is tagged
	// "errands" and has a body of "- soap", so a tag-only hit is unambiguous.
	byTag, err := s.SearchNotes(ctx, "ERRANDS", 0)
	if err != nil || len(byTag) != 1 {
		t.Fatalf("tag search: %v %v", byTag, err)
	}
	if byTag[0]["_id"] != otherID {
		t.Errorf("tag search returned %v, want the errands note", byTag[0]["_id"])
	}

	// `tags` must be []string on the READ path too, not just the insert path. A doc
	// decoded from Mongo carries a bson.A, so without docOut's normalisation this
	// assertion panics here — and passes in a test written against AddNote, which
	// is how the inconsistency survived the first two rounds of review.
	fetched, err := s.GetNote(ctx, id)
	if err != nil || fetched == nil {
		t.Fatalf("get: %v %v", fetched, err)
	}
	if _, ok := fetched["tags"].([]string); !ok {
		t.Errorf("GetNote tags is %T, want []string (bson.A leaks through docOut)", fetched["tags"])
	}
	if _, ok := byTag[0]["tags"].([]string); !ok {
		t.Errorf("SearchNotes tags is %T, want []string", byTag[0]["tags"])
	}

	// Patch semantics: absent keys are left alone. This is the regression that keeps
	// an edit to one line from wiping the others.
	if _, err := s.UpdateNote(ctx, id, map[string]any{"pinned": false}); err != nil {
		t.Fatalf("unpin: %v", err)
	}
	after, err := s.GetNote(ctx, id)
	if err != nil || after == nil {
		t.Fatalf("get: %v %v", after, err)
	}
	if after["body"] != "- milk\n- eggs\n\n- bread" || after["title"] != "gotest groceries" {
		t.Errorf("patch changed untouched fields: %v", after)
	}

	// Clearing a body is a real edit, not an absent arg. The store's patch map is what
	// the MCP layer fills from its pointers, so this asserts the store honours the
	// empty value rather than treating it as "not supplied".
	if _, err := s.UpdateNote(ctx, id, map[string]any{"body": ""}); err != nil {
		t.Fatalf("clear body: %v", err)
	}
	cleared, err := s.GetNote(ctx, id)
	if err != nil || cleared == nil {
		t.Fatalf("get after clear: %v %v", cleared, err)
	}
	if cleared["body"] != "" || cleared["title"] != "gotest groceries" {
		t.Errorf("body = %v, want cleared with the title untouched", cleared["body"])
	}

	// An empty patch must not write. `updatedAt` sorts a note to the top of every
	// list, so a no-op edit that bumped it would reorder the user's notes and imply a
	// change that never happened. The MCP layer can send an empty patch legitimately.
	// Read after the last real write, so the comparison below is against the current
	// value rather than one already superseded by the clearing edit above.
	before := cleared["updatedAt"]
	if _, err := s.UpdateNote(ctx, id, map[string]any{}); err != nil {
		t.Fatalf("empty patch: %v", err)
	}
	unchanged, err := s.GetNote(ctx, id)
	if err != nil || unchanged == nil {
		t.Fatalf("get after empty patch: %v %v", unchanged, err)
	}
	if unchanged["updatedAt"] != before {
		t.Errorf("empty patch bumped updatedAt: %v -> %v", before, unchanged["updatedAt"])
	}
	// An unknown key is not a silent write either.
	if _, err := s.UpdateNote(ctx, id, map[string]any{"not_a_field": "x"}); err != nil {
		t.Fatalf("unknown-key patch: %v", err)
	}
	if again, _ := s.GetNote(ctx, id); again["updatedAt"] != before {
		t.Errorf("a patch with no recognised field bumped updatedAt: %v", again["updatedAt"])
	}

	// A bad id is a domain error, and an id that simply does not exist is (nil, nil) —
	// but a DATABASE failure must never be reported as "unknown note". That is what
	// the ErrNoDocuments-only branch in GetNote exists for, and it is asserted here
	// with a closed client: every call fails at the socket, so any (nil, nil) coming
	// back is the swallowed-error bug.
	// A separate client, not a disconnected one: mongo.DB() is a process-cached
	// singleton, so pulling the plug on it would break every other test in this
	// package. An unroutable address with a short selection timeout fails the same way
	// a real outage does, and only for this client.
	broken := deadStore(t, ctx)
	if got, err := broken.GetNote(ctx, n["_id"].(string)); err == nil {
		t.Errorf("GetNote on a dead database returned (%v, nil) — a DB outage is being "+
			"reported as 'unknown note'", got)
	}
	// UpdateNote reaches GetNote, so it inherits the same guarantee.
	if got, err := broken.UpdateNote(ctx, n["_id"].(string), map[string]any{"pinned": true}); err == nil {
		t.Errorf("UpdateNote on a dead database returned (%v, nil)", got)
	}

	// The id is the only identifier: a title is not addressable.
	if _, err := s.GetNote(ctx, "gotest groceries"); err == nil {
		t.Error("expected a title lookup to be refused, not silently resolved")
	}

	ok, err := s.DeleteNote(ctx, id)
	if err != nil || !ok {
		t.Fatalf("delete: %v %v", ok, err)
	}
	if got, _ := s.GetNote(ctx, id); got != nil {
		t.Error("note survived delete")
	}
	// Deleting it again is not an error — deletion is idempotent — but it must report
	// "nothing was deleted" so the MCP layer can answer ok:false, matching get_note.
	if again, err := s.DeleteNote(ctx, id); err != nil || again {
		t.Errorf("second delete = (%v, %v), want (false, nil)", again, err)
	}
}

// deadStore returns a Store whose client cannot reach any server: an unroutable
// address with a short selection timeout, so calls fail in milliseconds instead of
// holding the test for the 30s context.
func deadStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	// 203.0.113.0/24 is TEST-NET-3, reserved and never routed.
	c, err := mongoDrv.Connect(ctx, options.Client().
		ApplyURI("mongodb://203.0.113.1:27017").
		SetServerSelectionTimeout(2*time.Second))
	if err != nil {
		t.Fatalf("connecting to the dead address: %v", err)
	}
	t.Cleanup(func() { _ = c.Disconnect(context.Background()) })
	return &Store{notes: c.Database("miser_test_notes").Collection("notes")}
}

func cleanup(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	hits, err := s.SearchNotes(ctx, "gotest", 0)
	if err != nil {
		t.Fatalf("cleanup search: %v", err)
	}
	for _, h := range hits {
		if _, err := s.DeleteNote(ctx, h["_id"].(string)); err != nil {
			t.Fatalf("cleanup delete: %v", err)
		}
	}
}

// A cap that splits a multi-byte rune writes invalid UTF-8 into the database. A note
// is the most likely place in this stack for a user to write an emoji or a non-Latin
// script, so this is the test that keeps truncation from corrupting one.
func TestTruncNeverSplitsARune(t *testing.T) {
	for _, tc := range []struct {
		s    string
		n    int
		want string
	}{
		{"plain ascii", 5, "plain"},
		{"café latte", 4, "caf"},
		// 4-byte runes: byte 5 is mid-rune, so the cut backs off to a boundary.
		{"🥑🥑🥑", 5, "🥑"},
		// 3-byte runes: n=4 lands inside the second one and yields 3 bytes, n=6 lands
		// exactly on the third rune's start and is kept.
		{"दूध", 4, "द"},
		{"दूध", 6, "दू"},
		{"ab", 40, "ab"},
	} {
		got := trunc(tc.s, tc.n)
		if got != tc.want {
			t.Errorf("trunc(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
		if !utf8.ValidString(got) {
			t.Errorf("trunc(%q, %d) = %q, which is not valid UTF-8", tc.s, tc.n, got)
		}
	}
}

func TestPreviewSkipsBlanksAndHeadings(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{"", ""},
		{"\n\n  \n- milk", "- milk"},
		// A heading repeats the title, which the summary already carries, so it is
		// skipped and the preview is the first thing that adds information.
		{"# Groceries\n\n- milk", "- milk"},
		{"# Groceries\n## Friday", ""}, // nothing but headings: nothing to preview
		{"  # indented heading\n- eggs", "- eggs"},
		// A '#' only starts a heading when a space or end-of-line follows it. These are
		// content: skipping them is how a note whose first line is a tag ends up
		// previewing as the wrong line entirely.
		{"#milk\n- eggs", "#milk"},
		{"#1 milk\n- eggs", "#1 milk"},
		// A bare "#" with nothing after it is an empty ATX heading, so it is skipped
		// like any other heading.
		{"#\n- eggs", "- eggs"},
	} {
		if got := preview(tc.body); got != tc.want {
			t.Errorf("preview(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}

func TestTruncRejectsNonPositiveLimits(t *testing.T) {
	// The rune-boundary loop indexes s[cut] while decrementing; a negative n walks it
	// off the front of the string. Every caller passes a positive constant today, which
	// is precisely why the guard belongs in trunc and not in the call sites.
	for _, n := range []int{0, -1, -80} {
		if got := trunc("milk", n); got != "" {
			t.Errorf("trunc(%q, %d) = %q, want \"\"", "milk", n, got)
		}
	}
	// And the flag that reports the cut must agree with what trunc actually did,
	// rather than being an independent guess at the same condition. `len(s) == n`
	// keeps every byte, so the flag is false there — an off-by-one would make the
	// app tell the user it was cut when it was not.
	if truncated("milk", 4) != false {
		t.Error("a string exactly at the cap is not truncated")
	}
	if truncated("milk", 3) != true {
		t.Error("truncated disagrees with trunc below the cap")
	}
	if truncated("milk", 40) != false {
		t.Error("truncated disagrees with trunc above the cap")
	}
	if trunc("milk", 3) != "mil" || len(trunc("milk", 3)) == len("milk") {
		t.Error("trunc did not actually cut below the cap")
	}
}

// An over-cap write is stored, but the response must admit it dropped bytes.
// Storing 20 KB of a 25 KB paste and answering ok:true is data loss reported as
// success — the user believes the whole thing is saved.
func TestOverCapInputIsStoredAndReported(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cleanup(t, s, ctx)

	long := strings.Repeat("x", maxBody+500)
	n, err := s.AddNote(ctx, "gotest long", long, nil, false)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	id := n["_id"].(string)
	defer func() { _, _ = s.DeleteNote(ctx, id) }()

	tr, ok := n["truncated"].(map[string]any)
	if !ok {
		t.Fatalf("truncated missing from the create response: %v", n)
	}
	if tr["body"] != true {
		t.Errorf("truncated.body = %v, want true", tr["body"])
	}
	if tr["title"] != false {
		t.Errorf("truncated.title = %v, want false for a short title", tr["title"])
	}
	// Stored, not rejected: the user's list is still there, just shorter.
	stored, err := s.GetNote(ctx, id)
	if err != nil || stored == nil {
		t.Fatalf("get: %v %v", stored, err)
	}
	if len(stored["body"].(string)) != maxBody {
		t.Errorf("stored body is %d bytes, want the capped %d", len(stored["body"].(string)), maxBody)
	}
	if !utf8.ValidString(stored["body"].(string)) {
		t.Error("the cap split a rune — stored body is not valid UTF-8")
	}

	// A short note reports nothing was cut.
	small, err := s.AddNote(ctx, "gotest short", "- milk", nil, false)
	if err != nil {
		t.Fatalf("add short: %v", err)
	}
	if tr, _ := small["truncated"].(map[string]any); tr["body"] != false || tr["tags"] != false {
		t.Errorf("a short note reported truncation: %v", tr)
	}
	_, _ = s.DeleteNote(ctx, small["_id"].(string))
}

// The truncation flag must describe what was actually STORED. Both of these caught
// the flag being computed somewhere other than the branch that did the write.
func TestUpdateTruncatedFlagMatchesWhatWasStored(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cleanup(t, s, ctx)

	n, err := s.AddNote(ctx, "gotest trunc", "- x", nil, false)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	id := n["_id"].(string)
	defer func() { _, _ = s.DeleteNote(ctx, id) }()

	// A padded title is trimmed before it is capped, so padding alone must not
	// report a cut — measuring the untrimmed input calls it truncated when the
	// stored value is short.
	padded := "   " + strings.Repeat("a", 10) + "   "
	out, err := s.UpdateNote(ctx, id, map[string]any{"title": padded})
	if err != nil {
		t.Fatalf("update title: %v", err)
	}
	if tr, _ := out["truncated"].(map[string]any); tr["title"] != false {
		t.Errorf("a padded short title reported truncated: %v", tr["title"])
	}
	if out["title"] != strings.Repeat("a", 10) {
		t.Errorf("title = %q, want the trimmed value", out["title"])
	}

	// A title that IS over the cap must report it.
	long := strings.Repeat("b", maxTitle+20)
	out, err = s.UpdateNote(ctx, id, map[string]any{"title": long})
	if err != nil {
		t.Fatalf("update long title: %v", err)
	}
	if tr, _ := out["truncated"].(map[string]any); tr["title"] != true {
		t.Errorf("an over-cap title reported truncated=%v, want true", tr["title"])
	}

	// Tags arriving as []any — which is what a BSON-decoded patch looks like —
	// must set the flag too. The earlier version only handled []string here, so
	// this path cleaned the tags and reported nothing.
	many := make([]any, maxTags+3)
	for i := range many {
		many[i] = fmt.Sprintf("k%02d", i)
	}
	out, err = s.UpdateNote(ctx, id, map[string]any{"tags": many})
	if err != nil {
		t.Fatalf("update tags: %v", err)
	}
	if tr, _ := out["truncated"].(map[string]any); tr["tags"] != true {
		t.Errorf("[]any over-cap tags reported truncated=%v, want true", tr["tags"])
	}
	if got := len(out["tags"].([]string)); got != maxTags {
		t.Errorf("stored %d tags, want the cap %d", got, maxTags)
	}

	// A []any patch under the cap reports nothing dropped.
	out, err = s.UpdateNote(ctx, id, map[string]any{"tags": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("update few tags: %v", err)
	}
	if tr, _ := out["truncated"].(map[string]any); tr["tags"] != false {
		t.Errorf("under-cap []any tags reported truncated=%v, want false", tr["tags"])
	}
}

// The cap is rune-aware, because the id is echoed into JSON and invalid UTF-8 in
// an MCP result is a broken payload, not a cosmetic problem. A raw byte slice here
// is exactly the bug trunc exists to prevent, and this function is shared with the
// MCP handlers precisely so there is only one copy of the rule.
func TestShortIDNeverSplitsARune(t *testing.T) {
	for _, in := range []string{
		"abc123",
		strings.Repeat("a", 80),  // exactly at the cap
		strings.Repeat("a", 200), // long ascii
		strings.Repeat("🥑", 40),  // 4-byte runes
		strings.Repeat("दू", 40), // 3-byte runes, two codepoints each
		strings.Repeat("日", 60),  // one codepoint per 3 bytes
	} {
		got := ShortID(in)
		if !utf8.ValidString(got) {
			t.Errorf("ShortID produced invalid UTF-8: %q", got)
		}
		if !strings.HasPrefix(in, strings.TrimSuffix(got, "…")) {
			t.Errorf("ShortID(%q) = %q, which is not a prefix of the input", in, got)
		}
		// The cap bounds the CONTENT; the ellipsis is a marker added on top, and
		// "…" is 3 bytes, so the total can exceed 80 by exactly that much.
		if len(got) > 80+len("…") {
			t.Errorf("ShortID(%d bytes in) = %d bytes, over the cap", len(in), len(got))
		}
	}
	// The two behaviours worth pinning exactly, rather than leaving to arithmetic:
	// short input is untouched, long input is marked.
	if got := ShortID("abc123"); got != "abc123" {
		t.Errorf("ShortID(short) = %q, want it unchanged", got)
	}
	long := strings.Repeat("a", 200)
	if got := ShortID(long); got != strings.Repeat("a", 80)+"…" {
		t.Errorf("ShortID(long ascii) = %q, want 80 a's plus an ellipsis", got)
	}
}

// A junk id must not be reflected back whole: the caller controls that string and
// it lands in the agent's context.
func TestBadIdIsCapped(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	huge := strings.Repeat("z", 5000)
	_, err := s.GetNote(ctx, huge)
	if err == nil {
		t.Fatal("expected a bad-id error")
	}
	if len(err.Error()) > 200 {
		t.Errorf("error is %d chars, want the id capped: %.80q...", len(err.Error()), err.Error())
	}
	// A plausible near-miss is still shown in full, so the message stays useful.
	near := strings.Repeat("a", 23)
	_, err = s.GetNote(ctx, near)
	if err == nil || !strings.Contains(err.Error(), near) {
		t.Errorf("error %q should contain the near-miss id %q", err, near)
	}
}

// Tags are the field most likely to be over-supplied (an agent enumerating a
// vocabulary), so their caps report like title and body rather than vanishing.
func TestTagCapsAreReported(t *testing.T) {
	long := strings.Repeat("x", maxTagLen+10)
	many := make([]string, maxTags+5)
	for i := range many {
		many[i] = fmt.Sprintf("t%02d", i)
	}

	for _, tc := range []struct {
		name string
		tags []string
		want bool
	}{
		{"none dropped", []string{"shopping", "errands"}, false},
		{"blank is sanitation not truncation", []string{"  ", "shopping"}, false},
		{"one tag over the length cap", []string{long}, true},
		{"more tags than the count cap", many, true},
	} {
		if got := tagsDropped(tc.tags); got != tc.want {
			t.Errorf("%s: tagsDropped = %v, want %v", tc.name, got, tc.want)
		}
	}

	// And the flag reaches the caller, not just the helper.
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cleanup(t, s, ctx)

	kept, err := s.AddNote(ctx, "gotest capped tags", "- x", many, false)
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	defer func() { _, _ = s.DeleteNote(ctx, kept["_id"].(string)) }()
	if tr, _ := kept["truncated"].(map[string]any); tr["tags"] != true {
		t.Errorf("truncated.tags = %v, want true", tr["tags"])
	}
	if got := len(kept["tags"].([]string)); got != maxTags {
		t.Errorf("stored %d tags, want the cap %d", got, maxTags)
	}
}
