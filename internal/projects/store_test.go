package projects

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"agento/internal/mongo"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		t.Skip("MONGODB_URI not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := mongo.ConnectURI(uri)
	if err != nil {
		t.Skipf("mongo unreachable: %v", err)
	}
	db := c.Database("projects_test")
	_ = db.Drop(ctx)
	s, err := New(uri, "projects_test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestCreateAndList(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc, err := s.Create(ctx, "Launch site", "", "hugo deploy")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if doc["name"] != "Launch site" || doc["status"] != "Todo" {
		t.Fatalf("unexpected doc: %v", doc)
	}
	rows, err := s.List(ctx, "", "", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List all: %v %v", rows, err)
	}
	rows, err = s.List(ctx, "Todo", "", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List Todo: %v %v", rows, err)
	}
	rows, err = s.List(ctx, "Done", "", 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("List Done should be empty: %v %v", rows, err)
	}
	rows, err = s.List(ctx, "", "launch", 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("List search: %v %v", rows, err)
	}
}

func TestValidation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "   ", "Todo", ""); err == nil {
		t.Fatal("blank name should fail")
	}
	if _, err := s.Create(ctx, "x", "Flying", ""); err == nil {
		t.Fatal("bad status should fail")
	}
	if _, err := s.List(ctx, "Flying", "", 0); err == nil {
		t.Fatal("bad list status should fail")
	}
	doc, err := s.Create(ctx, "x", "ongoing", "")
	if err != nil {
		t.Fatalf("lowercase status: %v", err)
	}
	if doc["status"] != "Ongoing" {
		t.Fatalf("status should canonicalize: %v", doc)
	}
}

func TestUpdateAndDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc, err := s.Create(ctx, "Ship it", "Todo", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id, _ := doc["id"].(string)
	updated, err := s.Update(ctx, id, map[string]any{"status": "Done", "note": "shipped"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if updated["status"] != "Done" || updated["note"] != "shipped" {
		t.Fatalf("unexpected doc: %v", updated)
	}
	got, err := s.Get(ctx, id)
	if err != nil || got["status"] != "Done" {
		t.Fatalf("Get: %v %v", got, err)
	}
	if _, err := s.Update(ctx, id, map[string]any{"name": "  "}); err == nil {
		t.Fatal("blank name should fail")
	}
	done, err := s.Delete(ctx, id)
	if err != nil || !done {
		t.Fatalf("Delete: %v %v", done, err)
	}
	done, err = s.Delete(ctx, id)
	if err != nil || done {
		t.Fatalf("second Delete should report missing: %v %v", done, err)
	}
	if _, err := s.Get(ctx, id); err == nil {
		t.Fatal("Get after Delete should fail")
	}
}

func TestListLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, name := range []string{"alpha", "beta", "gamma"} {
		if _, err := s.Create(ctx, name, "Todo", ""); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	rows, err := s.List(ctx, "", "", 2)
	if err != nil || len(rows) != 2 {
		t.Fatalf("List limit=2: %v %v", rows, err)
	}
	rows, err = s.List(ctx, "", "", 0)
	if err != nil || len(rows) != 3 {
		t.Fatalf("List limit=0 defaults: %v %v", rows, err)
	}
}

// #519: manual order. New rows append, reorder rewrites that section's
// positions, and reordering never bumps updatedAt.
func TestManualOrderAndReorder(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	var ids []string
	for _, name := range []string{"one", "two", "three"} {
		doc, err := s.Create(ctx, name, "Todo", "")
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		ids = append(ids, doc["id"].(string))
	}
	rows, err := s.List(ctx, "Todo", "", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := projectIDs(rows); !reflect.DeepEqual(got, ids) {
		t.Fatalf("creation order: got %v, want %v", got, ids)
	}
	before, err := s.Get(ctx, ids[0])
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := []string{ids[2], ids[0], ids[1]}
	if err := s.Reorder(ctx, "Todo", want); err != nil {
		t.Fatalf("Reorder: %v", err)
	}
	rows, err = s.List(ctx, "Todo", "", 0)
	if err != nil {
		t.Fatalf("List after reorder: %v", err)
	}
	if got := projectIDs(rows); !reflect.DeepEqual(got, want) {
		t.Fatalf("reordered ids: got %v, want %v", got, want)
	}
	after, err := s.Get(ctx, ids[0])
	if err != nil {
		t.Fatalf("Get after reorder: %v", err)
	}
	if before["updatedAt"] != after["updatedAt"] {
		t.Fatalf("reorder must not bump updatedAt: %v -> %v", before["updatedAt"], after["updatedAt"])
	}
	// Positions are spaced so a later create still appends at the end.
	doc, err := s.Create(ctx, "four", "Todo", "")
	if err != nil {
		t.Fatalf("Create after reorder: %v", err)
	}
	rows, err = s.List(ctx, "Todo", "", 0)
	if err != nil {
		t.Fatalf("List after append: %v", err)
	}
	if got := projectIDs(rows); len(got) != 4 || got[3] != doc["id"] {
		t.Fatalf("create after reorder must append: got %v", got)
	}
	// Rejections: wrong status, duplicate, unknown, blank.
	if err := s.Reorder(ctx, "Done", want); err == nil {
		t.Fatal("reorder with a status mismatch should fail")
	}
	if err := s.Reorder(ctx, "Todo", []string{ids[0], ids[0]}); err == nil {
		t.Fatal("duplicate ids should fail")
	}
	if err := s.Reorder(ctx, "Todo", []string{ids[0], "nope"}); err == nil {
		t.Fatal("unknown id should fail")
	}
	if err := s.Reorder(ctx, "Todo", []string{"  "}); err == nil {
		t.Fatal("blank id should fail")
	}
	if err := s.Reorder(ctx, "Todo", nil); err == nil {
		t.Fatal("empty ids should fail")
	}
}

// Rows written before #519 have no position and must sort after positioned
// rows instead of leading the board.
func TestLegacyRowsWithoutPositionSortLast(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	positioned, err := s.Create(ctx, "positioned", "Todo", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	now := primitive.NewDateTimeFromTime(time.Now().UTC())
	res, err := s.projects.InsertOne(ctx, bson.M{
		"name": "legacy", "status": "Todo", "note": "",
		"createdAt": now, "updatedAt": now,
	})
	if err != nil {
		t.Fatalf("insert legacy: %v", err)
	}
	legacy := res.InsertedID.(primitive.ObjectID).Hex()
	rows, err := s.List(ctx, "Todo", "", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := projectIDs(rows)
	if len(got) != 2 || got[0] != positioned["id"] || got[1] != legacy {
		t.Fatalf("legacy row must sort last: got %v", got)
	}
}

func projectIDs(rows []map[string]any) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		if id, ok := row["id"].(string); ok {
			out = append(out, id)
		}
	}
	return out
}
