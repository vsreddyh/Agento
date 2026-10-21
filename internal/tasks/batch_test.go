package tasks

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agento/internal/mongostore"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func mkBatchTask(t *testing.T, s *Store, name string, rep Repeat) string {
	t.Helper()
	doc, err := s.Create(context.Background(), name, "bulk test task", "2026-11-01", "09:00", intP(15), rep, boolP(false), "")
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}
	id, _ := doc["id"].(string)
	if id == "" {
		t.Fatalf("created task has no id: %v", doc)
	}
	return id
}

func openCount(t *testing.T, s *Store) int {
	t.Helper()
	rows, _, err := s.List(context.Background(), "open", false, "", 0)
	if err != nil {
		t.Fatalf("List open: %v", err)
	}
	return len(rows)
}

func revisionOf(t *testing.T, s *Store) int64 {
	t.Helper()
	rev, _, err := s.ChangeRevision(context.Background())
	if err != nil {
		t.Fatalf("ChangeRevision: %v", err)
	}
	return rev
}

func TestBatchCompletesAllAtomically(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := []string{
		mkBatchTask(t, s, "bulk-a", Repeat{}),
		mkBatchTask(t, s, "bulk-b", Repeat{}),
		mkBatchTask(t, s, "bulk-c", Repeat{}),
	}
	before := revisionOf(t, s)
	ops := []BatchOp{
		{Op: "complete", ID: ids[0]},
		{Op: "complete", ID: ids[1]},
		{Op: "complete", ID: ids[2]},
	}
	res, err := s.Batch(ctx, ops)
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 results, got %d", len(res))
	}
	for i, r := range res {
		if r.Task == nil {
			t.Fatalf("result %d has no task", i)
		}
		if r.Rollover != RolloverNone {
			t.Fatalf("result %d: one-shot rollover = %q, want none", i, r.Rollover)
		}
	}
	if n := openCount(t, s); n != 0 {
		t.Fatalf("all three must be done, %d still open", n)
	}
	if got := revisionOf(t, s) - before; got != 1 {
		t.Fatalf("one committed batch bumps exactly once, got %d", got)
	}
}

func TestBatchAbortsOnUnknownId(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := mkBatchTask(t, s, "bulk-a", Repeat{})
	b := mkBatchTask(t, s, "bulk-b", Repeat{})
	before := revisionOf(t, s)
	_, err := s.Batch(ctx, []BatchOp{
		{Op: "complete", ID: a},
		{Op: "complete", ID: primitive.NewObjectID().Hex()},
		{Op: "skip", ID: b, Reason: "busy"},
	})
	if err == nil {
		t.Fatal("unknown id must fail the batch")
	}
	bop, ok := err.(*BatchOpError)
	if !ok {
		t.Fatalf("want *BatchOpError, got %T (%v)", err, err)
	}
	if bop.Index != 1 {
		t.Fatalf("failing index = %d, want 1", bop.Index)
	}
	if n := openCount(t, s); n != 2 {
		t.Fatalf("aborted batch applies nothing, %d open (want 2)", n)
	}
	if got := revisionOf(t, s) - before; got != 0 {
		t.Fatalf("aborted batch bumps nothing, got %d", got)
	}
}

func TestBatchAbortsOnStaleRevision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := mkBatchTask(t, s, "bulk-a", Repeat{})
	b := mkBatchTask(t, s, "bulk-b", Repeat{})
	// Move b once so revision 0 is stale.
	if _, err := s.Update(ctx, b, map[string]any{"due_date": "2026-11-02"}); err != nil {
		t.Fatalf("setup Update: %v", err)
	}
	before := revisionOf(t, s)
	_, err := s.Batch(ctx, []BatchOp{
		{Op: "complete", ID: a},
		{Op: "update", ID: b, Fields: map[string]any{"due_date": "2026-11-03", "expected_revision": 0}},
	})
	if err == nil {
		t.Fatal("stale revision must fail the batch")
	}
	var se *StoreError
	bop, ok := err.(*BatchOpError)
	if !ok || bop.Index != 1 {
		t.Fatalf("want *BatchOpError at index 1, got %v", err)
	}
	if ok2 := errors.As(err, &se); !ok2 || !se.Conflict {
		t.Fatalf("stale revision must stay a conflict, got %v", err)
	}
	if n := openCount(t, s); n != 2 {
		t.Fatalf("aborted batch applies nothing, %d open (want 2)", n)
	}
	doc, _ := s.Get(ctx, a)
	if done, _ := doc["completedAt"].(string); done != "" {
		t.Fatalf("first op must roll back, task a = %v", doc["completedAt"])
	}
	if got := revisionOf(t, s) - before; got != 0 {
		t.Fatalf("aborted batch bumps nothing, got %d", got)
	}
}

func TestBatchValidationRejectsBadEnvelopes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := mkBatchTask(t, s, "bulk-a", Repeat{})
	cases := map[string][]BatchOp{
		"empty":   {},
		"unknown": {{Op: "vaporize", ID: id}},
		"bad id":  {{Op: "complete", ID: "not-an-id"}},
		"fields on complete": {
			{Op: "complete", ID: id, Fields: map[string]any{"due_date": "2026-12-01"}},
		},
		"reason on update": {
			{Op: "update", ID: id, Reason: "busy", Fields: map[string]any{"due_date": "2026-12-01"}},
		},
	}
	for name, ops := range cases {
		if _, err := s.Batch(ctx, ops); err == nil {
			t.Fatalf("%s: must fail", name)
		}
	}
	huge := make([]BatchOp, 0, BatchMaxOps+1)
	for i := 0; i < BatchMaxOps+1; i++ {
		huge = append(huge, BatchOp{Op: "complete", ID: id})
	}
	if _, err := s.Batch(ctx, huge); err == nil {
		t.Fatal("over-cap batch must fail")
	} else if !strings.Contains(err.Error(), "50") {
		t.Fatalf("cap error must name the cap, got %v", err)
	}
	if n := openCount(t, s); n != 1 {
		t.Fatalf("rejected batches apply nothing, %d open (want 1)", n)
	}
}

func TestBatchSkipRollsOverInside(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := mkBatchTask(t, s, "daily", Repeat{Every: 1, Unit: "days"})
	before := revisionOf(t, s)
	res, err := s.Batch(ctx, []BatchOp{{Op: "skip", ID: id, Reason: "busy"}})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(res) != 1 || res[0].Rollover != RolloverCreated {
		t.Fatalf("structured skip must roll over, got %+v", res)
	}
	if res[0].Next == nil {
		t.Fatal("created rollover must carry next")
	}
	if n := openCount(t, s); n != 1 {
		t.Fatalf("only the successor stays open, got %d", n)
	}
	if got := revisionOf(t, s) - before; got != 1 {
		t.Fatalf("rollover inside the batch still bumps exactly once, got %d", got)
	}
}

func TestBatchRolloverFailureAborts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	good := mkBatchTask(t, s, "good", Repeat{})
	broken := mkBatchTask(t, s, "broken", Repeat{Every: 1, Unit: "days"})
	// Corrupt the repeat past validation so the rollover plan fails: the
	// parent can still complete alone, but inside a batch that must abort.
	if _, err := s.tasks.UpdateOne(ctx, bson.M{"_id": mustOid(t, broken)},
		bson.M{"$set": bson.M{"repeat_every": 99}}); err != nil {
		t.Fatalf("corrupt fixture: %v", err)
	}
	before := revisionOf(t, s)
	_, err := s.Batch(ctx, []BatchOp{
		{Op: "complete", ID: good},
		{Op: "complete", ID: broken},
	})
	if err == nil {
		t.Fatal("rollover failure must abort the batch")
	}
	if n := openCount(t, s); n != 2 {
		t.Fatalf("aborted batch applies nothing, %d open (want 2)", n)
	}
	if got := revisionOf(t, s) - before; got != 0 {
		t.Fatalf("aborted batch bumps nothing, got %d", got)
	}
}

func TestBatchMoveUpdatesDueDates(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a := mkBatchTask(t, s, "bulk-a", Repeat{})
	b := mkBatchTask(t, s, "bulk-b", Repeat{})
	before := revisionOf(t, s)
	res, err := s.Batch(ctx, []BatchOp{
		{Op: "update", ID: a, Fields: map[string]any{"due_date": "2026-12-01"}},
		{Op: "update", ID: b, Fields: map[string]any{"due_date": "2026-12-01"}},
	})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	if res[0].Detail != "due_date" || res[1].Detail != "due_date" {
		t.Fatalf("update results carry the audit detail, got %q and %q", res[0].Detail, res[1].Detail)
	}
	for i, r := range res {
		if r.ID == "" || r.Task == nil {
			t.Fatalf("result %d must name its task, got %+v", i, r)
		}
	}
	for _, id := range []string{a, b} {
		doc, err := s.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		if doc["due_date"] != "2026-12-01" {
			t.Fatalf("task %s due_date = %v, want 2026-12-01", id, doc["due_date"])
		}
	}
	if got := revisionOf(t, s) - before; got != 1 {
		t.Fatalf("one committed batch bumps exactly once, got %d", got)
	}
}

func TestBatchNoOpUpdateSkipsBump(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := mkBatchTask(t, s, "bulk-a", Repeat{})
	doc, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rev, _ := mongostore.ToInt(doc["revision"])
	before := revisionOf(t, s)
	res, err := s.Batch(ctx, []BatchOp{
		{Op: "update", ID: id, Fields: map[string]any{"expected_revision": rev}},
	})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(res) != 1 || res[0].Detail != "no fields" {
		t.Fatalf("guard-only update is a no-op, got %+v", res)
	}
	if got := revisionOf(t, s) - before; got != 0 {
		t.Fatalf("all-no-op batch bumps nothing, got %d", got)
	}
	if n := openCount(t, s); n != 1 {
		t.Fatalf("task still open, got %d open", n)
	}
}

func mustOid(t *testing.T, hex string) primitive.ObjectID {
	t.Helper()
	oid, err := primitive.ObjectIDFromHex(hex)
	if err != nil {
		t.Fatalf("bad id %q: %v", hex, err)
	}
	return oid
}
