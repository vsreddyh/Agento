package tasks

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

// #176: `Create` was the one mutation that was not idempotent. A client retrying
// after a timeout got a SECOND task, and since the response is the new doc there
// was no way to tell the two apart or remove the duplicate in bulk.

// The property that matters is that the DATABASE decides, not a read-then-write.
// A test that only calls Create twice sequentially proves almost nothing: both
// implementations pass it. These tests race it.
func TestCreateWithKeyIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupIdem(t, s, ctx)

	first, replayed, err := s.CreateWithKey(ctx, "idem", "d", "2026-10-05", "08:00",
		intP(5), Repeat{}, boolP(false), "key-1", "test")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if replayed {
		t.Error("the FIRST use of a key must not be reported as a replay")
	}

	second, replayed, err := s.CreateWithKey(ctx, "idem", "d", "2026-10-05", "08:00",
		intP(5), Repeat{}, boolP(false), "key-1", "test")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !replayed {
		t.Error("a repeat of the same key must report replayed=true")
	}
	if second["id"] != first["id"] {
		t.Errorf("replay returned a DIFFERENT task: %v vs %v", second["id"], first["id"])
	}

	// And exactly one task exists. Counting is the check a read-then-write
	// implementation would fail.
	rows, _, err := s.List(ctx, "open", false, "idem", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("found %d tasks named 'idem', want 1 — the retry created a duplicate", len(rows))
	}
}

// Concurrent retries of the same key must produce ONE task. This is the case a
// pre-check cannot handle: every caller reads "nothing with this key" before any
// of them has inserted.
func TestCreateWithKeyConcurrentRetriesCreateOneTask(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupIdem(t, s, ctx)

	const callers = 8
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		ids   = map[string]int{}
		errs  []error
		reps  int
		start = make(chan struct{})
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release together, so the window is actually contended
			doc, replayed, err := s.CreateWithKey(ctx, "race", "d", "2026-10-05", "08:00",
				intP(5), Repeat{}, boolP(false), "race-key", "test")
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			if replayed {
				reps++
			}
			ids[fmt.Sprint(doc["id"])]++
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("concurrent creates errored: %v", errs)
	}
	if len(ids) != 1 {
		t.Errorf("%d concurrent retries with one key produced %d distinct tasks — "+
			"the idempotency guarantee is not enforced by the database", callers, len(ids))
	}
	// Exactly one caller may claim it created the task; the rest are replays.
	if reps != callers-1 {
		t.Errorf("replayed = %d of %d retries, want %d", reps, callers, callers-1)
	}

	rows, _, err := s.List(ctx, "open", false, "race", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("found %d tasks named 'race', want exactly 1", len(rows))
	}
}

// Distinct keys must NOT collide. Two different requests that happen to reuse a
// key are the caller's bug, but two DIFFERENT keys must always be two tasks —
// otherwise the guarantee is worse than useless, it eats real work.
func TestCreateWithDistinctKeysMakesDistinctTasks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupIdem(t, s, ctx)

	for i := 0; i < 3; i++ {
		if _, replayed, err := s.CreateWithKey(ctx, fmt.Sprintf("dist%d", i), "d",
			"2026-10-05", "08:00", intP(5), Repeat{}, boolP(false),
			fmt.Sprintf("dist-key-%d", i), "test"); err != nil || replayed {
			t.Fatalf("key %d: replayed=%v err=%v", i, replayed, err)
		}
	}
	rows, _, err := s.List(ctx, "open", false, "dist", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("found %d tasks, want 3 — distinct keys collided", len(rows))
	}
}

// Keyless creates must keep working, and must not collide with EACH OTHER. A
// unique index over a missing field would reject all but the first keyless task,
// which would be a far worse bug than the duplicate this whole thing prevents —
// hence the SPARSE index and this test.
func TestCreateWithoutAKeyStillWorksRepeatedly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupIdem(t, s, ctx)

	for i := 0; i < 3; i++ {
		if _, replayed, err := s.CreateWithKey(ctx, fmt.Sprintf("nokey%d", i), "d",
			"2026-10-05", "08:00", intP(5), Repeat{}, boolP(false), "", "test"); err != nil {
			t.Fatalf("keyless create %d: %v", i, err)
		} else if replayed {
			t.Errorf("keyless create %d reported as a replay", i)
		}
	}
	rows, _, err := s.List(ctx, "open", false, "nokey", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("found %d keyless tasks, want 3 — the sparse index is not sparse", len(rows))
	}
}

// The key and the caller are recorded, so a duplicate is traceable to a client.
// The agent and the app are indistinguishable at the auth layer (one shared
// password), which is exactly why nothing could be traced before.
func TestCreateRecordsKeyAndSource(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupIdem(t, s, ctx)

	if _, _, err := s.CreateWithKey(ctx, "traced", "d", "2026-10-05", "08:00",
		intP(5), Repeat{}, boolP(false), "trace-key", "app"); err != nil {
		t.Fatalf("create: %v", err)
	}
	var doc bson.M
	if err := s.tasks.FindOne(ctx, map[string]any{idempotencyKeyField: "trace-key"}).Decode(&doc); err != nil {
		t.Fatalf("direct read: %v", err)
	}
	if doc[idempotencyKeyField] != "trace-key" {
		t.Errorf("%s = %v", idempotencyKeyField, doc[idempotencyKeyField])
	}
	if doc[sourceField] != "app" {
		t.Errorf("%s = %v, want \"app\"", sourceField, doc[sourceField])
	}
}

// The mutation log must not break the mutation it is recording. A failed audit
// write is swallowed — the user asked for a task change, and refusing it because
// the record could not be written would be the worse outcome.
func TestRecordMutationNeverFailsTheCaller(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupIdem(t, s, ctx)

	id := mustCreate(t, s, "audited", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
	})
	s.RecordMutation(ctx, OpCreate, id, "test", "unit")
	var got bson.M
	if err := s.tasks.Database().Collection(auditCollection).
		FindOne(ctx, map[string]any{"task_id": id}).Decode(&got); err != nil {
		t.Fatalf("audit row not written: %v", err)
	}
	if got["op"] != OpCreate {
		t.Errorf("op = %v, want %q", got["op"], OpCreate)
	}
	if got["source"] != "test" {
		t.Errorf("source = %v, want \"test\"", got["source"])
	}
	if got["at"] == nil {
		t.Error("an audit row with no timestamp cannot be ordered or pruned")
	}

	// Degenerate inputs are dropped rather than stored as empty rows.
	s.RecordMutation(ctx, "", id, "test", "no op")
	s.RecordMutation(ctx, OpUpdate, "", "test", "no id")
	n, err := s.tasks.Database().Collection(auditCollection).
		CountDocuments(ctx, map[string]any{"task_id": id})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("%d audit rows for one task, want 1 — empty entries were stored", n)
	}
}

func cleanupIdem(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	// Only rows carrying an idempotency key or one of these names: the other task
	// tests share this database and their fixtures must survive.
	cur, err := s.tasks.Find(ctx, map[string]any{
		"$or": []map[string]any{
			{idempotencyKeyField: map[string]any{"$exists": true}},
			{"name": map[string]any{"$in": []string{"idem", "race", "dist0", "dist1", "dist2",
				"nokey0", "nokey1", "nokey2", "traced", "audited"}}},
		},
	})
	if err != nil {
		return
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return
	}
	for _, r := range rows {
		_, _ = s.Delete(ctx, fmt.Sprint(r["_id"]))
	}
	_, _ = s.tasks.Database().Collection(auditCollection).
		DeleteMany(ctx, map[string]any{"source": "test"})
}
