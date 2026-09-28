package tasks

import (
	"context"
	"testing"
	"time"

	"agento/internal/mongo"

	"go.mongodb.org/mongo-driver/bson"
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

func intP(n int) *int    { return &n }
func boolP(b bool) *bool { return &b }

func TestCreateAndList(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc, err := s.Create(ctx, "Pay rent", "bank transfer", "2026-10-01", "09:00", intP(15), "monthly on the 1st", boolP(false))
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
	if _, err := s.Create(ctx, "  ", "", "", "", intP(0), "", boolP(false)); err == nil {
		t.Fatal("blank name must fail")
	}
	if _, err := s.Create(ctx, "x", "", "10-01", "", intP(0), "", boolP(false)); err == nil {
		t.Fatal("bad due_date must fail")
	}
	if _, err := s.Create(ctx, "x", "", "2026-13-99", "", intP(0), "", boolP(false)); err == nil {
		t.Fatal("non-calendar due_date must fail")
	}
	if _, err := s.Create(ctx, "x", "", "", "9am", intP(0), "", boolP(false)); err == nil {
		t.Fatal("bad due_time must fail")
	}
	if _, err := s.Create(ctx, "x", "", "", "09:00", intP(0), "", boolP(false)); err == nil {
		t.Fatal("due_time without due_date must fail")
	}
	if _, err := s.Create(ctx, "x", "", "", "", intP(-5), "", boolP(false)); err == nil {
		t.Fatal("negative estimate must fail")
	}
}

func TestCreateRequiresAllFields(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	full := func() (string, string, string, string, *int, string, *bool) {
		return "job", "do the thing", "2026-10-05", "08:00", intP(30), "", boolP(false)
	}
	cases := map[string]func(string, string, string, string, *int, string, *bool) (string, string, string, string, *int, string, *bool){
		"empty description": func(n, d, dd, dt string, m *int, r string, p *bool) (string, string, string, string, *int, string, *bool) {
			return n, "  ", dd, dt, m, r, p
		},
		"empty due_date": func(n, d, dd, dt string, m *int, r string, p *bool) (string, string, string, string, *int, string, *bool) {
			return n, d, "", dt, m, r, p
		},
		"empty due_time": func(n, d, dd, dt string, m *int, r string, p *bool) (string, string, string, string, *int, string, *bool) {
			return n, d, dd, "", m, r, p
		},
		"nil estimated_minutes": func(n, d, dd, dt string, m *int, r string, p *bool) (string, string, string, string, *int, string, *bool) {
			return n, d, dd, dt, nil, r, p
		},
		"nil parallelable": func(n, d, dd, dt string, m *int, r string, p *bool) (string, string, string, string, *int, string, *bool) {
			return n, d, dd, dt, m, r, nil
		},
	}
	for name, mutate := range cases {
		n, d, dd, dt, m, r, p := mutate(full())
		if _, err := s.Create(ctx, n, d, dd, dt, m, r, p); err == nil {
			t.Fatalf("%s must fail", name)
		}
	}
	// Empty repeat_rule stays valid (one-shot).
	n, d, dd, dt, m, r, p := full()
	if _, err := s.Create(ctx, n, d, dd, dt, m, "", p); err != nil {
		t.Fatalf("empty repeat_rule (one-shot) must stay valid: %v", err)
	}
	// Clearing a required field via update must fail too.
	doc, err := s.Create(ctx, n+"-upd", d, dd, dt, m, r, p)
	if err != nil {
		t.Fatalf("setup create: %v", err)
	}
	id := doc["id"].(string)
	for _, fields := range []map[string]any{
		{"description": "  "},
		{"due_date": ""},
		{"due_time": ""},
	} {
		if _, err := s.Update(ctx, id, fields); err == nil {
			t.Fatalf("update %v must fail", fields)
		}
	}
	// Wrong types on update fail instead of silent no-ops.
	for _, fields := range []map[string]any{
		{"description": 42},
		{"due_date": 20261005},
		{"repeat_rule": true},
		{"parallelable": "yes"},
	} {
		if _, err := s.Update(ctx, id, fields); err == nil {
			t.Fatalf("update %v must fail", fields)
		}
	}
}

func TestCompleteReopenDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc, _ := s.Create(ctx, "Water plants", "balcony pots", "2026-10-05", "08:00", intP(5), "every Sunday", boolP(false))
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
	if _, err := s.Reopen(ctx, id); err == nil {
		t.Fatal("reopen of an open task must fail")
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

func TestReopenedExcludedFromDone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc, _ := s.Create(ctx, "reopen me", "test task", "2026-10-05", "08:00", intP(0), "", boolP(false))
	id := doc["id"].(string)
	if _, err := s.Complete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reopen(ctx, id); err != nil {
		t.Fatal(err)
	}
	// Reopen $unsets completedAt (field missing); bare $ne:null would
	// still match it — the done filter must exclude it.
	rows, err := s.List(ctx, "done", false, "")
	if err != nil || len(rows) != 0 {
		t.Fatalf("reopened task must not be in done list: %v %v", rows, err)
	}
	rows, err = s.List(ctx, "open", false, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("reopened task must be in open list: %v %v", rows, err)
	}
}

func TestSearchRegexCharsLiteral(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "fix (auth) [urgent]", "login flow", "2026-10-05", "08:00", intP(0), "", boolP(false)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List(ctx, "open", false, "(auth) [urgent]")
	if err != nil || len(rows) != 1 {
		t.Fatalf("regex metachars must match literally: %v %v", rows, err)
	}
}

func TestParallelableRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc, err := s.Create(ctx, "parallel job", "runs alongside others", "2026-10-05", "08:00", intP(0), "", boolP(true))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if doc["parallelable"] != true {
		t.Fatalf("create must store parallelable=true: %v", doc)
	}
	id := doc["id"].(string)
	off, err := s.Update(ctx, id, map[string]any{"parallelable": false})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if off["parallelable"] != false {
		t.Fatalf("update must clear parallelable: %v", off)
	}
	if _, err := s.Update(ctx, id, map[string]any{"parallelable": "yes"}); err == nil {
		t.Fatal("non-boolean parallelable must fail")
	}
}

func TestOverdueAndTTLIndex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	if _, err := s.Create(ctx, "late", "overdue task", yesterday, "08:00", intP(0), "", boolP(false)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "future", "upcoming task", tomorrow, "08:00", intP(0), "", boolP(false)); err != nil {
		t.Fatal(err)
	}
	rows, err := s.List(ctx, "open", true, "")
	if err != nil || len(rows) != 1 || rows[0]["name"] != "late" {
		t.Fatalf("overdue filter: %v %v", rows, err)
	}
	if _, err := s.List(ctx, "done", true, ""); err == nil {
		t.Fatal("overdue with state=done must fail")
	}
	cur, err := s.tasks.Indexes().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cur.Close(ctx)
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
}
