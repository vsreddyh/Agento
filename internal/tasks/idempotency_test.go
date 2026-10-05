package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
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
		intP(5), Repeat{}, boolP(false), "key-1", "test", "fp-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if replayed {
		t.Error("the FIRST use of a key must not be reported as a replay")
	}

	second, replayed, err := s.CreateWithKey(ctx, "idem", "d", "2026-10-05", "08:00",
		intP(5), Repeat{}, boolP(false), "key-1", "test", "fp-1")
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
				intP(5), Repeat{}, boolP(false), "race-key", "test", "fp-race")
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
			fmt.Sprintf("dist-key-%d", i), "test", "fp-dist"); err != nil || replayed {
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
			"2026-10-05", "08:00", intP(5), Repeat{}, boolP(false), "", "test", ""); err != nil {
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
		intP(5), Repeat{}, boolP(false), "trace-key", "app", "fp-trace"); err != nil {
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

// Reusing a key with a DIFFERENT body is not a replay. Answering 200 with the
// original task would be the worst possible reply: the caller would believe it
// created a task carrying the fields it just sent, and the task it gets back has
// none of them. That is a silent wrong answer, which is the entire failure mode
// the key was added to remove.
func TestCreateWithKeyRejectsADifferentBody(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupIdem(t, s, ctx)

	first, replayed, err := s.CreateWithKey(ctx, "original", "d", "2026-10-05", "08:00",
		intP(5), Repeat{}, boolP(false), "shared-key", "test", "fp-original")
	if err != nil || replayed {
		t.Fatalf("first create: replayed=%v err=%v", replayed, err)
	}

	_, replayed, err = s.CreateWithKey(ctx, "DIFFERENT", "d", "2026-10-05", "09:00",
		intP(30), Repeat{}, boolP(false), "shared-key", "test", "fp-different")
	if err == nil {
		t.Fatal("reusing a key with a different body must be refused, not replayed")
	}
	if replayed {
		t.Error("replayed=true on a mismatch — the caller must not be told it replayed")
	}
	var se *StoreError
	if !errors.As(err, &se) {
		t.Fatalf("error is %T, want *StoreError so the HTTP layer can map it", err)
	}
	if !se.Mismatch {
		t.Error("Mismatch flag not set — the handler cannot tell this from a validation failure")
	}
	if se.Conflict {
		t.Error("a key mismatch is not a revision race; it must not answer 409")
	}
	// And nothing was written: the count is the check a 200-with-wrong-task would fail.
	rows, _, err := s.List(ctx, "open", false, "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r["id"] == first["id"] {
			n++
		}
		if name, _ := r["name"].(string); name == "DIFFERENT" {
			t.Errorf("the mismatched body was inserted anyway: %v", r)
		}
	}
	if n != 1 {
		t.Errorf("the original task appears %d times, want 1", n)
	}
}

// The same key AND the same body is the retry case and must still replay — the
// mismatch check must not swallow the behaviour the key exists for.
func TestCreateWithKeyReplaysTheSameBody(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupIdem(t, s, ctx)

	first, _, err := s.CreateWithKey(ctx, "retry", "d", "2026-10-05", "08:00",
		intP(5), Repeat{}, boolP(false), "retry-key", "test", "fp-same")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, replayed, err := s.CreateWithKey(ctx, "retry", "d", "2026-10-05", "08:00",
		intP(5), Repeat{}, boolP(false), "retry-key", "test", "fp-same")
	if err != nil {
		t.Fatalf("a genuine retry must replay, got: %v", err)
	}
	if !replayed {
		t.Error("replayed = false for a genuine retry")
	}
	if second["id"] != first["id"] {
		t.Errorf("replay returned a different task: %v vs %v", second["id"], first["id"])
	}
}

// A row written before the fingerprint field existed has none, and so does a caller
// with no body to digest (the MCP path). Treating "unknown" as "different" would
// reject every legacy key on its first retry — the opposite of what the key is for.
func TestCreateWithKeyToleratesAMissingFingerprint(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupIdem(t, s, ctx)

	// A row with a key and NO fingerprint, as written before this field existed.
	legacy := bson.M{
		"name": "legacy", "description": "d",
		"due_date": "2026-10-05", "due_time": "08:00",
		"estimated_minutes": 5, "parallelable": false,
		"completedAt": nil, "revision": 0,
		idempotencyKeyField: "legacy-key",
	}
	res, err := s.tasks.InsertOne(ctx, legacy)
	if err != nil {
		t.Fatalf("insert legacy: %v", err)
	}
	legacyID := res.InsertedID.(primitive.ObjectID).Hex()

	// Replaying a legacy key WITH a fingerprint must not be refused.
	doc, replayed, err := s.CreateWithKey(ctx, "totally different", "d", "2026-11-11", "23:00",
		intP(99), Repeat{}, boolP(false), "legacy-key", "test", "fp-new")
	if err != nil {
		t.Fatalf("a legacy key must still replay, got: %v", err)
	}
	if !replayed {
		t.Error("replayed = false for a legacy key")
	}
	if doc["id"] != legacyID {
		t.Errorf("replay returned %v, want the legacy task %v", doc["id"], legacyID)
	}
}

// The fingerprint is written in the SAME insert as the key. A task carrying one
// without the other would mean a row that can be replayed without being checked.
func TestFingerprintIsStoredWithTheKey(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupIdem(t, s, ctx)

	if _, _, err := s.CreateWithKey(ctx, "fp", "d", "2026-10-05", "08:00",
		intP(5), Repeat{}, boolP(false), "fp-key", "test", "fp-abc123"); err != nil {
		t.Fatalf("create: %v", err)
	}
	var doc bson.M
	if err := s.tasks.FindOne(ctx, bson.M{idempotencyKeyField: "fp-key"}).Decode(&doc); err != nil {
		t.Fatalf("read: %v", err)
	}
	if doc[idempotencyFingerprintField] != "fp-abc123" {
		t.Errorf("%s = %v, want %q", idempotencyFingerprintField,
			doc[idempotencyFingerprintField], "fp-abc123")
	}
}

// The rune-safe clamp: a cut on a byte boundary inside a multi-byte rune would store
// invalid UTF-8, which BSON accepts and every reader then has to defend against.
// Three of the four clamps in this package were byte-slicing; one was not.
func TestTruncateRunesNeverSplitsARune(t *testing.T) {
	const max = 40
	cases := []struct {
		name string
		in   string
	}{
		{"all multi-byte, over the cap", strings.Repeat("é", 60)},
		{"emoji, surrogate pairs", strings.Repeat("🙂", 40)},
		{"mixed ascii and wide", strings.Repeat("aé", 40)},
		{"devanagari, 3-byte runes", strings.Repeat("क", 40)},
	}
	for _, c := range cases {
		got := truncateRunes(c.in, max)
		if len(got) > max {
			t.Errorf("%s: length %d exceeds the %d-byte cap", c.name, len(got), max)
		}
		if !utf8.ValidString(got) {
			t.Errorf("%s: produced invalid UTF-8: %q", c.name, got)
		}
		if len(c.in) > max && got == "" {
			t.Errorf("%s: truncated everything, so the cap did not back off to a rune boundary", c.name)
		}
	}
	// Under the cap, nothing is touched.
	if got := truncateRunes("short", max); got != "short" {
		t.Errorf("a short string was altered: %q", got)
	}
}
