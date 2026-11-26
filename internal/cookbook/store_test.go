package cookbook

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
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

func TestDeleteUnknownReadsNotDeleted(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Deletes are idempotent across both books: unknown targets read back
	// as not-deleted, while gets/updates report them as ok:false.
	done, err := s.DeleteIngredient(ctx, "gotest-no-such-ingredient")
	if err != nil || done {
		t.Fatalf("got %v %v", done, err)
	}
	done, err = s.DeleteRecipe(ctx, "gotest-no-such-recipe")
	if err != nil || done {
		t.Fatalf("got %v %v", done, err)
	}
}

func TestEmptyPatchIsRejected(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, _ = s.DeleteIngredient(ctx, "gotest-patch")
	if _, err := s.AddIngredient(ctx, "gotest-patch", "keep"); err != nil {
		t.Fatalf("add: %v", err)
	}
	defer func() { _, _ = s.DeleteIngredient(ctx, "gotest-patch") }()
	if _, err := s.UpdateIngredient(ctx, "gotest-patch", map[string]any{}); err == nil {
		t.Fatal("expected nothing-to-update error")
	}
	// Whitespace-only renames are dropped, which also leaves nothing.
	if _, err := s.UpdateIngredient(ctx, "gotest-patch", map[string]any{"name": "   "}); err == nil {
		t.Fatal("expected nothing-to-update error")
	}
	got, err := s.GetIngredient(ctx, "gotest-patch")
	if err != nil || got == nil || got["name"] != "gotest-patch" {
		t.Fatalf("got %v %v", got, err)
	}
}

func TestLegacyRecipeFieldsStrippedOnRead(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, _ = s.DeleteRecipe(ctx, "gotest-legacy")
	legacy := bson.M{"name": "gotest-legacy", "servings": 2.0,
		"kcal": 100.0, "protein_g": 5.0, "carbs_g": 1.0, "fat_g": 2.0, "fiber_g": 3.0,
		"quantities": []bson.M{{"ingredient_id": "abc", "name": "salt", "qty": "2"}},
		"source":     "mcp", "note": "old", "tags": []string{"t"}}
	if _, err := s.recipes.InsertOne(ctx, legacy); err != nil {
		t.Fatalf("insert legacy: %v", err)
	}
	defer func() { _, _ = s.DeleteRecipe(ctx, "gotest-legacy") }()
	r, err := s.GetRecipe(ctx, "gotest-legacy")
	if err != nil || r == nil {
		t.Fatalf("got %v %v", r, err)
	}
	for _, k := range []string{"servings", "quantities", "source",
		"kcal", "protein_g", "carbs_g", "fat_g", "fiber_g"} {
		if _, ok := r[k]; ok {
			t.Fatalf("legacy key %q still returned: %v", k, r)
		}
	}
	if r["note"] != "old" {
		t.Fatalf("got %v", r)
	}
}
