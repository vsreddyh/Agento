package tasks

import (
	"context"
	"strings"
	"testing"
	"time"

	"agento/internal/mongostore"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestDuplicateCopiesContents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	src, err := s.Create(ctx, "Gym", "leg day", "2026-11-04", "08:00", intP(30),
		Repeat{Every: 1, Unit: "days"}, boolP(true), "gym time")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	srcID, _ := src["id"].(string)
	doc, err := s.Duplicate(ctx, srcID)
	if err != nil {
		t.Fatalf("Duplicate: %v", err)
	}
	newID, _ := doc["id"].(string)
	if newID == "" || newID == srcID {
		t.Fatalf("duplicate must mint a new id, got %q (src %q)", newID, srcID)
	}
	for _, k := range []string{"name", "description", "speak_name", "due_date", "due_time",
		"estimated_minutes", "parallelable",
		"repeat_every", "repeat_unit", "repeat_rule", "repeat_custom"} {
		if doc[k] != src[k] {
			t.Fatalf("%s: got %v, want %v", k, doc[k], src[k])
		}
	}
	if doc["repeat_days"] == nil {
		t.Fatalf("repeat_days must copy as an explicit list, got nil")
	}
	if doc["completedAt"] != nil {
		t.Fatalf("duplicate must be open, got completedAt=%v", doc["completedAt"])
	}
	if rev, _ := mongostore.ToInt(doc["revision"]); rev != 0 {
		t.Fatalf("duplicate starts at revision 0, got %v", doc["revision"])
	}
	if _, ok := doc["rolled_from"]; ok {
		t.Fatalf("a user copy is not a rollover successor: %v", doc["rolled_from"])
	}
}

func TestDuplicateUnknownId(t *testing.T) {
	s := testStore(t)
	if _, err := s.Duplicate(context.Background(), primitive.NewObjectID().Hex()); err == nil {
		t.Fatal("unknown id must fail")
	} else if !strings.Contains(err.Error(), "unknown task") {
		t.Fatalf("must read as unknown task, got %v", err)
	}
}

func TestDuplicateDoneTaskCopiesAsOpen(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := mkBatchTask(t, s, "one-shot", Repeat{})
	if _, _, _, _, err := s.CompleteDetail(ctx, id); err != nil {
		t.Fatalf("CompleteDetail: %v", err)
	}
	doc, err := s.Duplicate(ctx, id)
	if err != nil {
		t.Fatalf("Duplicate: %v", err)
	}
	if doc["completedAt"] != nil {
		t.Fatalf("duplicate of a done task must be open, got %v", doc["completedAt"])
	}
}

func TestBatchDuplicateAtomically(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := mkBatchTask(t, s, "bulk-a", Repeat{})
	b := mkBatchTask(t, s, "bulk-b", Repeat{Every: 1, Unit: "weeks"})
	before := revisionOf(t, s)
	res, err := s.Batch(ctx, []BatchOp{{Op: "duplicate", ID: a}, {Op: "duplicate", ID: b}})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	for i, r := range res {
		if r.Task == nil || r.Rollover != RolloverNone {
			t.Fatalf("result %d: want task with no rollover, got %+v", i, r)
		}
		if r.Task["completedAt"] != nil {
			t.Fatalf("result %d must be open", i)
		}
		newID, _ := r.Task["id"].(string)
		if newID == "" {
			t.Fatalf("result %d: copy has no id", i)
		}
		// The result names the row produced, not the row asked about:
		// the copy is a different id, and the response envelope and the
		// audit log both read ID.
		if r.ID != newID {
			t.Fatalf("result %d: ID = %q, want new copy %q", i, r.ID, newID)
		}
	}
	if n := openCount(t, s); n != 4 {
		t.Fatalf("2 sources + 2 copies = 4 open, got %d", n)
	}
	if got := revisionOf(t, s) - before; got != 1 {
		t.Fatalf("one committed batch bumps exactly once, got %d", got)
	}
}

func TestBatchDuplicateAborts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := mkBatchTask(t, s, "bulk-a", Repeat{})
	before := revisionOf(t, s)
	_, err := s.Batch(ctx, []BatchOp{
		{Op: "duplicate", ID: a},
		{Op: "duplicate", ID: primitive.NewObjectID().Hex()},
	})
	if err == nil {
		t.Fatal("bogus duplicate must abort the batch")
	}
	if n := openCount(t, s); n != 1 {
		t.Fatalf("aborted batch creates nothing, %d open (want 1)", n)
	}
	if got := revisionOf(t, s) - before; got != 0 {
		t.Fatalf("aborted batch bumps nothing, got %d", got)
	}
}

func TestDuplicateLegacyRowWithoutNumbers(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// A pre-backfill row: no estimated_minutes, no parallelable. The user
	// can still complete it, so duplicating must work too — reading absent
	// as zero, like a missing revision. Present-but-wrong-typed still fails.
	res, err := s.tasks.InsertOne(ctx, bson.M{
		"name": "legacy", "description": "old row",
		"due_date": "2026-11-04", "due_time": "08:00",
		"revision": int32(0), "completedAt": nil,
		"createdAt": primitive.NewDateTimeFromTime(time.Now().UTC()),
	})
	if err != nil {
		t.Fatalf("insert fixture: %v", err)
	}
	id := res.InsertedID.(primitive.ObjectID).Hex()
	doc, err := s.Duplicate(ctx, id)
	if err != nil {
		t.Fatalf("Duplicate: %v", err)
	}
	if mins, ok := mongostore.ToInt(doc["estimated_minutes"]); !ok || mins != 0 {
		t.Fatalf("absent estimate duplicates as 0, got %v", doc["estimated_minutes"])
	}
	if _, err := s.tasks.UpdateOne(ctx, bson.M{"_id": mustOid(t, id)},
		bson.M{"$set": bson.M{"estimated_minutes": "lots"}}); err != nil {
		t.Fatalf("corrupt fixture: %v", err)
	}
	if _, err := s.Duplicate(ctx, id); err == nil {
		t.Fatal("wrong-typed estimate must fail, not duplicate as zero")
	}
}
