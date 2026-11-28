package cookbook

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

func TestTruncateBytes(t *testing.T) {
	if got := truncateBytes("abcdef", 4); got != "abcd" {
		t.Fatalf("got %q", got)
	}
	if got := truncateBytes("abc", 10); got != "abc" {
		t.Fatalf("got %q", got)
	}
	// "é" is 2 bytes: a 3-byte cap must back off to 2, not store half a rune.
	if got := truncateBytes("aé", 2); got != "a" {
		t.Fatalf("got %q", got)
	}
	if got := truncateBytes("aé", 3); got != "aé" {
		t.Fatalf("got %q", got)
	}
	// 4-byte emoji: cap inside the rune backs off to the boundary.
	if got := truncateBytes("ab😀", 5); got != "ab" {
		t.Fatalf("got %q", got)
	}
}

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
	rows, total, err := s.ListIngredients(ctx, "gotest-s", "", "")
	if err != nil || len(rows) != 1 || total != 1 {
		t.Fatalf("got %v total=%d %v", rows, total, err)
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
	// Notes trim like names: padding is stripped, whitespace-only reads as empty.
	upd, err = s.UpdateIngredient(ctx, "gotest-salt", map[string]any{"note": "  padded  "})
	if err != nil || upd == nil || upd["note"] != "padded" {
		t.Fatalf("got %v %v", upd, err)
	}
	if _, err := s.UpdateIngredient(ctx, "gotest-salt", map[string]any{"note": "   "}); err == nil {
		t.Fatal("expected nothing-to-update for whitespace-only note")
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
	rows, total, err := s.ListRecipes(ctx, "gotest-s", "test", "", "")
	if err != nil || len(rows) != 1 || total != 1 {
		t.Fatalf("got %v total=%d %v", rows, total, err)
	}
	rows, total, err = s.ListRecipes(ctx, "", "no-such-tag", "", "")
	if err != nil || len(rows) != 0 || total != 0 {
		t.Fatalf("got %v total=%d %v", rows, total, err)
	}
	upd, err := s.UpdateRecipe(ctx, "gotest-soup", map[string]any{"tags": []any{"test", "fast"}})
	if err != nil || upd == nil {
		t.Fatalf("got %v %v", upd, err)
	}
	// Whitespace-only tags read as empty and are left alone, never wiping the list.
	if _, err := s.UpdateRecipe(ctx, "gotest-soup", map[string]any{"tags": []any{"", "   "}}); err == nil {
		t.Fatal("expected nothing-to-update for whitespace-only tags")
	}
	got, err := s.GetRecipe(ctx, "gotest-soup")
	if err != nil || got == nil {
		t.Fatalf("got %v %v", got, err)
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

// List sort: two enums, not one string. sort_by names the field
// (name|updatedAt), sort the direction (asc|desc); blank means the defaults
// (name, asc). Unknown values fail on the store layer naming the allowed set.
func TestListSort(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, n := range []string{"gotest-sort-b", "gotest-sort-a"} {
		_, _ = s.DeleteIngredient(ctx, n)
	}
	defer func() {
		_, _ = s.DeleteIngredient(ctx, "gotest-sort-a")
		_, _ = s.DeleteIngredient(ctx, "gotest-sort-b")
	}()
	if _, err := s.AddIngredient(ctx, "gotest-sort-b", ""); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := s.AddIngredient(ctx, "gotest-sort-a", ""); err != nil {
		t.Fatalf("add: %v", err)
	}
	rows, total, err := s.ListIngredients(ctx, "gotest-sort-", "", "")
	if err != nil || len(rows) != 2 || total != 2 {
		t.Fatalf("got %v total=%d %v", rows, total, err)
	}
	if rows[0]["name"] != "gotest-sort-a" || rows[1]["name"] != "gotest-sort-b" {
		t.Fatalf("default order is name-asc, got %v, %v", rows[0]["name"], rows[1]["name"])
	}
	rows, total, err = s.ListIngredients(ctx, "gotest-sort-", "name", "desc")
	if err != nil || len(rows) != 2 {
		t.Fatalf("got %v %v", rows, err)
	}
	if rows[0]["name"] != "gotest-sort-b" || rows[1]["name"] != "gotest-sort-a" {
		t.Fatalf("name-desc swapped nothing: %v, %v", rows[0]["name"], rows[1]["name"])
	}
	if _, _, err := s.ListIngredients(ctx, "", "colour", ""); err == nil {
		t.Fatal("unknown sort_by was accepted")
	} else if !strings.Contains(err.Error(), "sort_by must be one of") {
		t.Fatalf("sort_by error %q does not name the allowed set", err)
	}
	if _, _, err := s.ListIngredients(ctx, "", "", "sideways"); err == nil {
		t.Fatal("unknown sort was accepted")
	} else if !strings.Contains(err.Error(), "sort must be one of") {
		t.Fatalf("sort error %q does not name the allowed set", err)
	}
	if _, _, err := s.ListRecipes(ctx, "", "", "colour", ""); err == nil {
		t.Fatal("unknown recipe sort_by was accepted")
	}
	if _, _, err := s.ListRecipes(ctx, "", "", "", "sideways"); err == nil {
		t.Fatal("unknown recipe sort was accepted")
	}
	// Whitespace-only counts as absent (the default), like money's
	// normalization — not as an error.
	rows, total, err = s.ListIngredients(ctx, "gotest-sort-", "   ", "  ")
	if err != nil || len(rows) != 2 {
		t.Fatalf("got %v %v", rows, err)
	}
	if rows[0]["name"] != "gotest-sort-a" {
		t.Fatalf("whitespace sort did not default to name-asc: %v", rows[0]["name"])
	}
	// The direction folds case like money's order; the field names stay
	// case-exact (updatedAt is camelCase, so lowercasing would corrupt it).
	rows, total, err = s.ListIngredients(ctx, "gotest-sort-", "", "DESC")
	if err != nil || len(rows) != 2 {
		t.Fatalf("got %v %v", rows, err)
	}
	if rows[0]["name"] != "gotest-sort-b" {
		t.Fatalf("DESC did not order name-desc: %v", rows[0]["name"])
	}
	if _, _, err := s.ListIngredients(ctx, "", "UpdatedAt", ""); err == nil {
		t.Fatal("cased sort_by was accepted; field names are case-sensitive")
	}

	for _, n := range []string{"gotest-rsort-b", "gotest-rsort-a"} {
		_, _ = s.DeleteRecipe(ctx, n)
	}
	defer func() {
		_, _ = s.DeleteRecipe(ctx, "gotest-rsort-a")
		_, _ = s.DeleteRecipe(ctx, "gotest-rsort-b")
	}()
	if _, err := s.AddRecipe(ctx, "gotest-rsort-b", "", nil); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := s.AddRecipe(ctx, "gotest-rsort-a", "", nil); err != nil {
		t.Fatalf("add: %v", err)
	}
	rows, total, err = s.ListRecipes(ctx, "gotest-rsort-", "", "name", "desc")
	if err != nil || len(rows) != 2 {
		t.Fatalf("got %v %v", rows, err)
	}
	if rows[0]["name"] != "gotest-rsort-b" || rows[1]["name"] != "gotest-rsort-a" {
		t.Fatalf("recipe name-desc swapped nothing: %v, %v", rows[0]["name"], rows[1]["name"])
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
