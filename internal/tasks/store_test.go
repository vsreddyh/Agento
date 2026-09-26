package tasks

import (
	"context"
	"testing"
	"time"

	"agento/internal/mongo"

	"go.mongodb.org/mongo-driver/bson"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"os"
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
	db := c.Database("tasks_test")
	_ = db.Drop(ctx)
	s, err := New(uri, "tasks_test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestCreateAndList(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc, err := s.Create(ctx, "Pay rent", "bank transfer", "2026-10-01", "09:00", 15, "monthly on the 1st")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if doc["name"] != "Pay rent" || doc["repeat_rule"] != "monthly on the 1st" {
		t.Fatalf("unexpected doc: %v", doc)
	}
	if _, ok := doc["completedAt"]; ok && doc["completedAt"] != nil {
		t.Fatalf("new task must be open: %v", doc)
	}
	rows, err := s.List(ctx, "open", false, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("List open: %v %v", rows, err)
	}
	rows, err = s.List(ctx, "done", false, "")
	if err != nil || len(rows) != 0 {
		t.Fatalf("List done should be empty: %v %v", rows, err)
	}
}

func TestValidation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "  ", "", "", "", 0, ""); err == nil {
		t.Fatal("blank name must fail")
	}
	if _, err := s.Create(ctx, "x", "", "10-01", "", 0, ""); err == nil {
		t.Fatal("bad due_date must fail")
	}
	if _, err := s.Create(ctx, "x", "", "", "9am", 0, ""); err == nil {
		t.Fatal("bad due_time must fail")
	}
	if _, err := s.Create(ctx, "x", "", "", "09:00", 0, ""); err == nil {
		t.Fatal("due_time without due_date must fail")
	}
	if _, err := s.Create(ctx, "x", "", "", "", -5, ""); err == nil {
		t.Fatal("negative estimate must fail")
	}
}

func TestCompleteReopenDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc, _ := s.Create(ctx, "Water plants", "", "", "", 5, "every Sunday")
	id := doc["id"].(string)

	done, err := s.Complete(ctx, id)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if done["repeat_rule"] != "every Sunday" {
		t.Fatalf("complete must echo repeat_rule: %v", done)
	}
	if done["completedAt"] == nil || done["expiresAt"] == nil {
		t.Fatalf("complete must set completedAt+expiresAt: %v", done)
	}
	if _, err := s.Complete(ctx, id); err == nil {
		t.Fatal("double complete must fail")
	}
	rows, _ := s.List(ctx, "open", false, "")
	if len(rows) != 0 {
		t.Fatalf("completed task must leave open list: %v", rows)
	}

	open, err := s.Reopen(ctx, id)
	if err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if _, hasExpiry := open["expiresAt"]; hasExpiry {
		t.Fatalf("reopen must clear expiresAt: %v", open)
	}
	rows, _ = s.List(ctx, "open", false, "")
	if len(rows) != 1 {
		t.Fatalf("reopened task must be open again: %v", rows)
	}

	ok, err := s.Delete(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Delete: %v %v", ok, err)
	}
	if _, err := s.Get(ctx, id); err == nil {
		t.Fatal("deleted task must be gone")
	}
}

func TestOverdueAndTTLIndex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	if _, err := s.Create(ctx, "late", "", yesterday, "", 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "future", "", tomorrow, "", 0, ""); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List(ctx, "open", true, "")
	if err != nil || len(rows) != 1 || rows[0]["name"] != "late" {
		t.Fatalf("overdue filter: %v %v", rows, err)
	}
	cur, err := s.tasks.Indexes().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for cur.Next(ctx) {
		var idx bson.M
		_ = cur.Decode(&idx)
		names = append(names, idx["name"].(string))
	}
	for _, want := range []string{"ttl_expiresAt", "completedAt_1", "due_date_1"} {
		found := false
		for _, n := range names {
			if n == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing index %s (have %v)", want, names)
		}
	}
	_ = mongoDrv.ErrNoDocuments
}
