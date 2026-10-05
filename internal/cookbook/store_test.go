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
	// not a test. internal/tasks, internal/projects and internal/notes already have
	// their own; this closes the last two.
	t.Setenv("MONGODB_DB", "cookbook_test")
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestIngredientRecipeRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := s.AddIngredient(ctx, "", ""); err == nil {
		t.Fatal("expected empty-name error")
	}
	// Idempotent: clear leftovers from interrupted runs.
	_, _, _ = s.DeleteRecipe(ctx, "gotest-soup")
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
	macros := map[string]any{
		"kcal": 100.0, "protein_g": 5.0, "carbs_g": 10.0, "fat_g": 2.0, "fiber_g": 1.0}
	r, err := s.AddRecipe(ctx, "gotest-soup", map[string]any{"gotest-salt": "2 spoons"},
		macros, 2, "", []string{"test"}, "mcp")
	if err != nil {
		t.Fatalf("add recipe: %v", err)
	}
	if _, err := s.DeleteIngredient(ctx, "gotest-salt"); err == nil {
		t.Fatal("expected ref-in-use error")
	}
	sc, err := s.ScaleRecipe(ctx, "gotest-soup", 4)
	if err != nil || sc["factor"] != 2.0 {
		t.Fatalf("got %v %v", sc, err)
	}
	c, err := s.LogCook(ctx, "gotest-soup", "extra stir", "", "")
	if err != nil {
		t.Fatalf("log cook: %v", err)
	}
	if c["recipe_name"] != "gotest-soup" {
		t.Fatalf("got %v", c)
	}
	cooks, err := s.ListCooks(ctx, "gotest-soup", 10)
	if err != nil || len(cooks) != 1 {
		t.Fatalf("got %v %v", cooks, err)
	}
	deleted, logs, err := s.DeleteRecipe(ctx, idstr(r["_id"]))
	if err != nil || !deleted || logs != 1 {
		t.Fatalf("got %v %v %v", deleted, logs, err)
	}
	done, err := s.DeleteIngredient(ctx, "gotest-salt")
	if err != nil || !done {
		t.Fatalf("got %v %v", done, err)
	}
}

func TestMacroValidation(t *testing.T) {
	if _, err := checkMacros(map[string]any{"kcal": 1}); err == nil {
		t.Fatal("expected missing-macro error")
	}
	if _, err := checkMacros(map[string]any{
		"kcal": -1.0, "protein_g": 5.0, "carbs_g": 10.0, "fat_g": 2.0, "fiber_g": 1.0}); err == nil {
		t.Fatal("expected negative error")
	}
}
