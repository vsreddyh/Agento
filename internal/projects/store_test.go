package projects

import (
	"context"
	"os"
	"testing"
	"time"

	"agento/internal/mongo"
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
	rows, err := s.List(ctx, "", "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("List all: %v %v", rows, err)
	}
	rows, err = s.List(ctx, "Todo", "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("List Todo: %v %v", rows, err)
	}
	rows, err = s.List(ctx, "Done", "")
	if err != nil || len(rows) != 0 {
		t.Fatalf("List Done should be empty: %v %v", rows, err)
	}
	rows, err = s.List(ctx, "", "launch")
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
	if _, err := s.List(ctx, "Flying", ""); err == nil {
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
