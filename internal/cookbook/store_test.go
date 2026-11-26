package cookbook

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
	// Point at the throwaway test database like the Python suite does.
	t.Setenv("MONGODB_URI", os.Getenv("MONGODB_URI"))
	// Its OWN throwaway database, not the shared `miser_test`. `go test ./...` runs
	// packages in parallel and internal/money calls db.Drop(ctx) on `miser_test` — a
	// wholesale drop, not a scoped delete. Sharing it made this suite fail whenever it
	// happened to run alongside money: the collection was dropped between a write and
	// the read that asserted it, and the test reported empty results with no error
	// anywhere. A test that fails based on which package the scheduler paired it with is
	// not a test. internal/tasks and internal/projects already have
	// their own; this closes the last two.
	t.Setenv("MONGODB_DB", "cookbook_test")
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestIngredientCRUD(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := s.AddIngredient(ctx, "", ""); err == nil {
		t.Fatal("expected empty-name error")
	}
	// Idempotent: clear leftovers from interrupted runs.
	_, _ = s.DeleteIngredient(ctx, "gotest-salt")
	if _, err := s.AddIngredient(ctx, "gotest-salt", "test"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := s.AddIngredient(ctx, "gotest-salt", ""); err == nil {
		t.Fatal("expected duplicate error")
	}
	rows, err := s.ListIngredients(ctx, "gotest-s")
	if err != nil || len(rows) != 1 {
		t.Fatalf("got %v %v", rows, err)
	}
	got, err := s.GetIngredient(ctx, "gotest-salt")
	if err != nil || got == nil || got["note"] != "test" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := s.GetIngredient(ctx, "gotest-missing"); err != nil {
		t.Fatalf("get unknown should return nil, nil: %v", err)
	}
	upd, err := s.UpdateIngredient(ctx, "gotest-salt", map[string]any{"note": "flaky"})
	if err != nil || upd == nil || upd["note"] != "flaky" {
		t.Fatalf("got %v %v", upd, err)
	}
	if upd["updatedAt"] == nil {
		t.Fatal("expected updatedAt to be stamped")
	}
	done, err := s.DeleteIngredient(ctx, "gotest-salt")
	if err != nil || !done {
		t.Fatalf("got %v %v", done, err)
	}
}

func TestRecipeCRUD(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := s.AddRecipe(ctx, "", "", nil); err == nil {
		t.Fatal("expected empty-name error")
	}
	// Idempotent: clear leftovers from interrupted runs.
	_, _ = s.DeleteRecipe(ctx, "gotest-soup")
	r, err := s.AddRecipe(ctx, "gotest-soup", "weeknight", []string{"test"})
	if err != nil {
		t.Fatalf("add recipe: %v", err)
	}
	if r["note"] != "weeknight" {
		t.Fatalf("got %v", r)
	}
	if _, err := s.AddRecipe(ctx, "gotest-soup", "", nil); err == nil {
		t.Fatal("expected duplicate error")
	}
	rows, err := s.ListRecipes(ctx, "gotest-s", "test")
	if err != nil || len(rows) != 1 {
		t.Fatalf("got %v %v", rows, err)
	}
	rows, err = s.ListRecipes(ctx, "", "no-such-tag")
	if err != nil || len(rows) != 0 {
		t.Fatalf("got %v %v", rows, err)
	}
	upd, err := s.UpdateRecipe(ctx, "gotest-soup", map[string]any{"tags": []any{"test", "fast"}})
	if err != nil || upd == nil {
		t.Fatalf("got %v %v", upd, err)
	}
	deleted, err := s.DeleteRecipe(ctx, idstr(r["_id"]))
	if err != nil || !deleted {
		t.Fatalf("got %v %v", deleted, err)
	}
}
