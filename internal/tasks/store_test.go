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

// customRep is the free-text repeat mode: the user's words, verbatim.
func customRep(text string) Repeat { return Repeat{Custom: true, Text: text} }

func TestCreateAndList(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc, err := s.Create(ctx, "Pay rent", "bank transfer", "2026-10-01", "09:00", intP(15), customRep("monthly on the 1st"), boolP(false))
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
	if _, err := s.Create(ctx, "  ", "", "", "", intP(0), Repeat{}, boolP(false)); err == nil {
		t.Fatal("blank name must fail")
	}
	if _, err := s.Create(ctx, "x", "", "10-01", "", intP(0), Repeat{}, boolP(false)); err == nil {
		t.Fatal("bad due_date must fail")
	}
	if _, err := s.Create(ctx, "x", "", "2026-13-99", "", intP(0), Repeat{}, boolP(false)); err == nil {
		t.Fatal("non-calendar due_date must fail")
	}
	if _, err := s.Create(ctx, "x", "", "", "9am", intP(0), Repeat{}, boolP(false)); err == nil {
		t.Fatal("bad due_time must fail")
	}
	if _, err := s.Create(ctx, "x", "", "", "09:00", intP(0), Repeat{}, boolP(false)); err == nil {
		t.Fatal("due_time without due_date must fail")
	}
	if _, err := s.Create(ctx, "x", "", "", "", intP(-5), Repeat{}, boolP(false)); err == nil {
		t.Fatal("negative estimate must fail")
	}
}

func TestCreateRequiresAllFields(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	full := func() (string, string, string, string, *int, Repeat, *bool) {
		return "job", "do the thing", "2026-10-05", "08:00", intP(30), Repeat{}, boolP(false)
	}
	cases := map[string]func(string, string, string, string, *int, Repeat, *bool) (string, string, string, string, *int, Repeat, *bool){
		"empty description": func(n, d, dd, dt string, m *int, r Repeat, p *bool) (string, string, string, string, *int, Repeat, *bool) {
			return n, "  ", dd, dt, m, r, p
		},
		"empty due_date": func(n, d, dd, dt string, m *int, r Repeat, p *bool) (string, string, string, string, *int, Repeat, *bool) {
			return n, d, "", dt, m, r, p
		},
		"empty due_time": func(n, d, dd, dt string, m *int, r Repeat, p *bool) (string, string, string, string, *int, Repeat, *bool) {
			return n, d, dd, "", m, r, p
		},
		"nil estimated_minutes": func(n, d, dd, dt string, m *int, r Repeat, p *bool) (string, string, string, string, *int, Repeat, *bool) {
			return n, d, dd, dt, nil, r, p
		},
		"nil parallelable": func(n, d, dd, dt string, m *int, r Repeat, p *bool) (string, string, string, string, *int, Repeat, *bool) {
			return n, d, dd, dt, m, r, nil
		},
	}
	for name, mutate := range cases {
		n, d, dd, dt, m, r, p := mutate(full())
		if _, err := s.Create(ctx, n, d, dd, dt, m, r, p); err == nil {
			t.Fatalf("%s must fail", name)
		}
	}
	// The all-zero repeat stays valid (one-shot).
	n, d, dd, dt, m, r, p := full()
	if _, err := s.Create(ctx, n, d, dd, dt, m, Repeat{}, p); err != nil {
		t.Fatalf("zero repeat (one-shot) must stay valid: %v", err)
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
	// JSON null counts as absent on update (no-op success).
	if _, err := s.Update(ctx, id, map[string]any{
		"name": nil, "description": nil, "estimated_minutes": nil, "parallelable": nil,
	}); err != nil {
		t.Fatalf("null update must be a no-op: %v", err)
	}
}

func TestCompleteReopenDelete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc, _ := s.Create(ctx, "Water plants", "balcony pots", "2026-10-05", "08:00", intP(5), customRep("every Sunday"), boolP(false))
	id := doc["id"].(string)

	done, next, err := s.Complete(ctx, id)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if done["repeat_rule"] != "every Sunday" {
		t.Fatalf("complete must echo repeat_rule: %v", done)
	}
	if done["completedAt"] == nil || done["expiresAt"] == nil {
		t.Fatalf("complete must set completedAt+expiresAt: %v", done)
	}
	// "every Sunday" is a custom condition: the server must not invent the
	// next date, so nothing is created behind the caller's back.
	if next != nil {
		t.Fatalf("custom repeat must not roll over server-side: %v", next)
	}
	if _, _, err := s.Complete(ctx, id); err == nil {
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
	doc, _ := s.Create(ctx, "reopen me", "test task", "2026-10-05", "08:00", intP(0), Repeat{}, boolP(false))
	id := doc["id"].(string)
	if _, _, err := s.Complete(ctx, id); err != nil {
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
	if _, err := s.Create(ctx, "fix (auth) [urgent]", "login flow", "2026-10-05", "08:00", intP(0), Repeat{}, boolP(false)); err != nil {
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
	doc, err := s.Create(ctx, "parallel job", "runs alongside others", "2026-10-05", "08:00", intP(0), Repeat{}, boolP(true))
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

// The rollover date arithmetic, without a database. Month-end clamping and
// the catch-up loop are where this would otherwise go quietly wrong.
func TestNextDueDate(t *testing.T) {
	today := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	// day() is the "as of" date. Month-end cases use a today just before
	// the task is due, so exactly one step is taken; the catch-up rows use
	// a real today and assert the loop.
	day := func(y int, m time.Month, d int) time.Time {
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	cases := []struct {
		name string
		rep  Repeat
		from string
		want string
		asOf time.Time
	}{
		{"every day", Repeat{Every: 1, Unit: "days"}, "2026-09-29", "2026-09-30", today},
		{"every 3 days", Repeat{Every: 3, Unit: "days"}, "2026-09-29", "2026-10-02", today},
		{"every 2 weeks", Repeat{Every: 2, Unit: "weeks"}, "2026-09-29", "2026-10-13", today},
		{"every week", Repeat{Every: 1, Unit: "weeks"}, "2026-09-29", "2026-10-06", today},
		{"every month", Repeat{Every: 1, Unit: "months"}, "2026-09-29", "2026-10-29", today},
		{"every 6 months", Repeat{Every: 6, Unit: "months"}, "2026-05-15", "2026-11-15", today},
		{"every year", Repeat{Every: 1, Unit: "years"}, "2026-09-29", "2027-09-29", today},
		// Month ends clamp instead of overflowing: Go's AddDate would turn
		// 31 Jan + 1 month into 3 March, which is a different date.
		{"31 Jan + 1 month", Repeat{Every: 1, Unit: "months"}, "2026-01-31", "2026-02-28", day(2026, 1, 15)},
		{"31 Jan 2028 + 1 month (leap)", Repeat{Every: 1, Unit: "months"}, "2028-01-31", "2028-02-29", day(2028, 1, 15)},
		{"31 Mar + 1 month", Repeat{Every: 1, Unit: "months"}, "2026-03-31", "2026-04-30", day(2026, 3, 15)},
		{"30 Apr + 1 month", Repeat{Every: 1, Unit: "months"}, "2026-04-30", "2026-05-30", day(2026, 4, 15)},
		{"29 Feb + 1 year", Repeat{Every: 1, Unit: "years"}, "2028-02-29", "2029-02-28", day(2028, 2, 15)},
		{"31 Dec + 1 month rolls the year", Repeat{Every: 1, Unit: "months"}, "2026-12-31", "2027-01-31", day(2026, 12, 15)},
		{"every 2 months across a year end", Repeat{Every: 2, Unit: "months"}, "2026-11-30", "2027-01-30", day(2026, 11, 15)},
		// A long-overdue task must land in the future, not stay overdue.
		{"catch up from 4 months ago", Repeat{Every: 1, Unit: "months"}, "2026-05-01", "2026-10-01", today},
		{"catch up from last year", Repeat{Every: 1, Unit: "days"}, "2025-09-01", "2026-09-29", today},
		{"catch up skips a past step", Repeat{Every: 6, Unit: "months"}, "2026-01-15", "2027-01-15", today},
		{"catch up keeps month ends clamped", Repeat{Every: 1, Unit: "months"}, "2026-01-31", "2026-10-28", today},
	}
	for _, c := range cases {
		got, ok := c.rep.NextDueDate(c.from, c.asOf)
		if !ok || got != c.want {
			t.Fatalf("%s: NextDueDate(%q) = %q/%v, want %q", c.name, c.from, got, ok, c.want)
		}
	}
	// Custom and one-shot have no computable next date: those are the
	// caller's to work out, and guessing is worse than asking.
	for _, rep := range []Repeat{
		{},
		customRep("mon-fri only"),
		{Every: 2, Unit: "days", Custom: true, Text: "sort of"},
	} {
		if got, ok := rep.NextDueDate("2026-09-29", today); ok {
			t.Fatalf("%+v must not roll over, got %q", rep, got)
		}
	}
	// A malformed date is skipped rather than guessed at.
	if _, ok := (Repeat{Every: 1, Unit: "days"}).NextDueDate("tomorrow", today); ok {
		t.Fatal("unparseable due_date must not roll over")
	}
}

func TestRepeatValidate(t *testing.T) {
	cases := []struct {
		name string
		rep  Repeat
		ok   bool
	}{
		{"one-shot", Repeat{}, true},
		{"structured", Repeat{Every: 3, Unit: "days"}, true},
		{"every 28 days is the top of the range", Repeat{Every: 28, Unit: "days"}, true},
		{"every 1 day", Repeat{Every: 1, Unit: "days"}, true},
		{"every unit accepted", Repeat{Every: 2, Unit: "years"}, true},
		{"custom", customRep("mon-fri only"), true},
		// Pre-4.6 clients send text with no flag: inferred as custom.
		{"text alone becomes custom", Repeat{Text: "end of month"}, true},
		{"29 is out of range", Repeat{Every: 29, Unit: "days"}, false},
		{"0 with a unit", Repeat{Every: 0, Unit: "days"}, false},
		{"negative count", Repeat{Every: -1, Unit: "days"}, false},
		{"unknown unit", Repeat{Every: 2, Unit: "fortnights"}, false},
		{"missing unit", Repeat{Every: 2}, false},
		{"custom with no words", Repeat{Custom: true}, false},
		{"both modes at once", Repeat{Every: 2, Unit: "days", Custom: true, Text: "mostly"}, false},
		{"cadence with stray text", Repeat{Every: 2, Unit: "days", Text: "sometimes"}, false},
	}
	for _, c := range cases {
		err := c.rep.Validate()
		if c.ok && err != nil {
			t.Fatalf("%s must be valid: %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("%s must be rejected", c.name)
		}
	}
}

func TestRepeatString(t *testing.T) {
	cases := map[string]Repeat{
		"":                   {},
		"Every day":          {Every: 1, Unit: "days"},
		"Every 3 days":       {Every: 3, Unit: "days"},
		"Every 2 weeks":      {Every: 2, Unit: "weeks"},
		"Every 6 months":     {Every: 6, Unit: "months"},
		"Every year":         {Every: 1, Unit: "years"},
		"mon-fri only":       {Custom: true, Text: "mon-fri only"},
		"end of every month": {Text: "end of every month"},
	}
	for want, rep := range cases {
		if got := rep.String(); got != want {
			t.Fatalf("String() = %q, want %q", got, want)
		}
	}
}

func TestMergeRepeat(t *testing.T) {
	// A stand-in for the store's own string reader, so the merge rules
	// can be exercised without touching Mongo.
	str := func(fields map[string]any) func(string) (string, bool) {
		return func(key string) (string, bool) {
			s, ok := fields[key].(string)
			return s, ok
		}
	}
	cases := []struct {
		name    string
		cur     bson.M
		fields  map[string]any
		touched bool
		want    Repeat
		wantErr bool
	}{
		{
			name:   "unrelated edit leaves the rule alone",
			cur:    bson.M{"repeat_every": 3, "repeat_unit": "days"},
			fields: map[string]any{"name": "x"},
		},
		{
			name:    "legacy text-only write is a custom condition",
			cur:     bson.M{},
			fields:  map[string]any{"repeat_rule": "mon-fri only"},
			touched: true,
			want:    customRep("mon-fri only"),
		},
		{
			// A pre-4.6 client can only say "clear the rule" by sending
			// empty text; honouring that must not strand a cadence it
			// cannot see.
			name:    "empty text from an old client clears the whole rule",
			cur:     bson.M{"repeat_every": 3, "repeat_unit": "days", "repeat_custom": false},
			fields:  map[string]any{"repeat_rule": ""},
			touched: true,
			want:    Repeat{},
		},
		{
			name: "explicitly blanking every key clears it",
			cur:  bson.M{"repeat_every": 3, "repeat_unit": "days"},
			fields: map[string]any{
				"repeat_every": 0, "repeat_unit": "",
				"repeat_custom": false, "repeat_rule": "",
			},
			touched: true,
			want:    Repeat{},
		},
		{
			name:    "switching to a cadence drops the custom words",
			cur:     bson.M{"repeat_rule": "mon-fri only", "repeat_custom": true},
			fields:  map[string]any{"repeat_every": 2, "repeat_unit": "weeks", "repeat_custom": false, "repeat_rule": ""},
			touched: true,
			want:    Repeat{Every: 2, Unit: "weeks"},
		},
		{
			name:    "counting the cadence up keeps the unit",
			cur:     bson.M{"repeat_every": 2, "repeat_unit": "weeks"},
			fields:  map[string]any{"repeat_every": 4},
			touched: true,
			want:    Repeat{Every: 4, Unit: "weeks"},
		},
		{
			name:    "out-of-range count is rejected",
			cur:     bson.M{},
			fields:  map[string]any{"repeat_every": 30, "repeat_unit": "days"},
			touched: true,
			wantErr: true,
		},
		{
			name:    "wrongly typed count is rejected",
			cur:     bson.M{},
			fields:  map[string]any{"repeat_every": "three"},
			touched: true,
			wantErr: true,
		},
	}
	for _, c := range cases {
		got, touched, err := mergeRepeat(c.cur, c.fields, str(c.fields))
		if c.wantErr {
			if err == nil {
				t.Fatalf("%s: must fail", c.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if touched != c.touched {
			t.Fatalf("%s: touched = %v, want %v", c.name, touched, c.touched)
		}
		if !c.touched {
			continue
		}
		if got != c.want {
			t.Fatalf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestLegacyDocBackfill(t *testing.T) {
	// No DB: toDoc is pure, and the legacy shape (no due_time, no
	// parallelable) is exactly what pre-mandatory rows look like. The
	// contract that matters: reads never invent a time, and a doc with
	// no date+time pair still passes checkDue so it can be edited.
	doc := toDoc(bson.M{
		"name":        "Read mails",
		"due_date":    "2026-10-05",
		"completedAt": nil,
	})
	if doc["due_time"] != "" {
		t.Fatalf("legacy doc must read back an empty time, not an invented one: %v", doc)
	}
	if doc["parallelable"] != false {
		t.Fatalf("legacy doc must read back parallelable=false: %v", doc)
	}
	if doc["repeat_rule"] != "" || doc["description"] != "" {
		t.Fatalf("legacy doc must backfill every mandatory key: %v", doc)
	}
	// A time-less legacy row must stay updatable: the user supplies the
	// time on first edit rather than being locked out of the task.
	if err := checkDue("2026-10-05", ""); err != nil {
		t.Fatalf("legacy date+empty time must stay valid to edit: %v", err)
	}
}

func TestOverdueAndTTLIndex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	if _, err := s.Create(ctx, "late", "overdue task", yesterday, "08:00", intP(0), Repeat{}, boolP(false)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "future", "upcoming task", tomorrow, "08:00", intP(0), Repeat{}, boolP(false)); err != nil {
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
