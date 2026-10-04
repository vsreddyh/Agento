package notes

import (
	"context"
	"os"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("MONGODB_URI") == "" {
		t.Skip("MONGODB_URI not set")
	}
	// Throwaway test database, same convention as the cookbook suite.
	t.Setenv("MONGODB_URI", os.Getenv("MONGODB_URI"))
	t.Setenv("MONGODB_DB", "miser_test")
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
		t.Error("list_notes returned a body — the projection is missing")
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
