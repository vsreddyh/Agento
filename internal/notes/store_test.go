package notes

import (
	"context"
	"os"
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

	// Pinned-first ordering: the pinned note is this one, so add an unpinned rival.
	other, err := s.AddNote(ctx, "gotest unpinned", "- soap", []string{"test"}, false)
	if err != nil {
		t.Fatalf("add rival: %v", err)
	}
	defer func() { _, _ = s.DeleteNote(ctx, other["_id"].(string)) }()
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
		{"# Groceries\n\n- milk", "# Groceries"},
	} {
		if got := preview(tc.body); got != tc.want {
			t.Errorf("preview(%q) = %q, want %q", tc.body, got, tc.want)
		}
	}
}
