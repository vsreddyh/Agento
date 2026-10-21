package tasks

import (
	"context"
	"strings"
	"testing"
	"time"
)

// The op filter is a filter, not a free-text search: an op the log does not
// hold is a caller mistake, and saying so beats an empty history that reads
// like "nothing happened".
func TestParseOpsAcceptsKnownOps(t *testing.T) {
	ops, err := ParseOps("complete, skip")
	if err != nil {
		t.Fatalf("ParseOps: %v", err)
	}
	if len(ops) != 2 || ops[0] != OpComplete || ops[1] != OpSkip {
		t.Fatalf("ops = %v, want [complete skip]", ops)
	}
}

func TestParseOpsRejectsUnknownOp(t *testing.T) {
	_, err := ParseOps("complete,frobnicate")
	if err == nil {
		t.Fatal("unknown op must be rejected")
	}
	// The message must name the offending op AND the valid ones, so the
	// caller can fix the call without reading the source.
	if !strings.Contains(err.Error(), "frobnicate") {
		t.Errorf("error must name the bad op: %v", err)
	}
	if !strings.Contains(err.Error(), OpComplete) {
		t.Errorf("error must list the known ops: %v", err)
	}
}

// An empty filter is "every op", not "no ops": the whole log is the default
// reading of the question.
func TestParseOpsEmptyMeansEveryOp(t *testing.T) {
	ops, err := ParseOps("   ")
	if err != nil {
		t.Fatalf("ParseOps: %v", err)
	}
	if len(ops) != 0 {
		t.Fatalf("ops = %v, want none", ops)
	}
}

// A repeated op is one op: the filter is a set, and a duplicate in it is a
// caller being redundant, not a different question.
func TestParseOpsDropsRepeats(t *testing.T) {
	ops, err := ParseOps("create,create,update")
	if err != nil {
		t.Fatalf("ParseOps: %v", err)
	}
	if len(ops) != 2 || ops[0] != OpCreate || ops[1] != OpUpdate {
		t.Fatalf("ops = %v, want [create update]", ops)
	}
}

// A filter that names NO op is a filter the caller sent and got the whole
// log for — the "wrong data, not merely more of it" the strict filters
// exist to prevent.
func TestParseOpsRejectsAFilterThatNamesNoOp(t *testing.T) {
	for _, raw := range []string{",", ",,"} {
		if _, err := ParseOps(raw); err == nil {
			t.Errorf("ParseOps(%q) must be rejected, not read as the whole log", raw)
		}
	}
	// A stray comma beside a real op is tolerated: it names one op.
	ops, err := ParseOps("complete,")
	if err != nil {
		t.Fatalf("ParseOps: %v", err)
	}
	if len(ops) != 1 || ops[0] != OpComplete {
		t.Fatalf("ops = %v, want [complete]", ops)
	}
}

// The op set is the validation rule, so a caller must not be able to edit it
// for everyone else by accident.
func TestKnownOpsHandsOutACopy(t *testing.T) {
	ops := KnownOps()
	if len(ops) == 0 {
		t.Fatal("KnownOps is empty")
	}
	ops[0] = "frobnicate"
	// The rule itself must be untouched.
	if err := ValidateOps([]string{KnownOps()[0]}); err != nil {
		t.Errorf("editing a copy changed the validation set: %v", err)
	}
}

// The validator normalizes too, so it agrees with the parser and the store
// seam: an exported check that disagreed with the thing it checks would be a
// second rule. And it echoes what the caller SENT, not the normalized copy.
func TestValidateOpsNormalizes(t *testing.T) {
	if err := ValidateOps([]string{"CREATE", " complete ", ""}); err != nil {
		t.Errorf("an op in the wrong case must be accepted: %v", err)
	}
	err := ValidateOps([]string{" FROBNICATE "})
	if err == nil {
		t.Fatal("an unknown op must be rejected")
	}
	if !strings.Contains(err.Error(), "FROBNICATE") {
		t.Errorf("the error must echo what the caller sent: %v", err)
	}
}

// The `since` filter is fed back from the `at` the read path hands out, so
// it must accept that shape — milliseconds included — and reject a value
// that is not a timestamp at all.
func TestParseSince(t *testing.T) {
	// Empty means no bound.
	at, err := ParseSince("  ")
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if !at.IsZero() {
		t.Errorf("at = %v, want the zero time", at)
	}
	// Plain RFC3339.
	at, err = ParseSince("2026-10-21T00:00:00Z")
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if at.Year() != 2026 || at.Month() != time.October || at.Day() != 21 {
		t.Errorf("at = %v, want 2026-10-21", at)
	}
	// With milliseconds — the format the log's own timestamps come back in.
	at, err = ParseSince("2026-10-21T00:00:00.123Z")
	if err != nil {
		t.Fatalf("ParseSince: %v", err)
	}
	if at.Nanosecond() != 123000000 {
		t.Errorf("at = %v, want the milliseconds kept", at)
	}
	// Not a timestamp.
	if _, err := ParseSince("yesterday"); err == nil {
		t.Error("a non-timestamp must be rejected")
	}
}

// A malformed task id can only ever match nothing, and an empty history
// reads like "this task was never touched" — so it is rejected, by name.
func TestNormalizeTaskID(t *testing.T) {
	id, err := NormalizeTaskID("  0123456789abcdef01234567  ")
	if err != nil {
		t.Fatalf("NormalizeTaskID: %v", err)
	}
	if id != "0123456789abcdef01234567" {
		t.Errorf("id = %q, want it trimmed", id)
	}
	// Empty means no filter, not an error.
	id, err = NormalizeTaskID("")
	if err != nil {
		t.Fatalf("NormalizeTaskID: %v", err)
	}
	if id != "" {
		t.Errorf("id = %q, want empty", id)
	}
	if _, err := NormalizeTaskID("not-an-id"); err == nil {
		t.Error("a malformed id must be rejected")
	}
}

// The stored ids are lowercase, so an id in upper case is the SAME task and
// must answer with the same history. A check that only validated the shape
// would pass the id and then match nothing — the empty history that reads
// like "never touched", which is the whole reason the id is checked.
func TestNormalizeTaskIDLowercases(t *testing.T) {
	id, err := NormalizeTaskID("0123456789ABCDEF01234567")
	if err != nil {
		t.Fatalf("NormalizeTaskID: %v", err)
	}
	if id != "0123456789abcdef01234567" {
		t.Errorf("id = %q, want it lowercased", id)
	}
}

// The store is the seam: a caller that reaches it directly must not be able
// to ask about a malformed id and get an empty history back.
func TestListMutationsRejectsMalformedTaskIDWithoutTouchingMongo(t *testing.T) {
	var s Store
	_, _, err := s.ListMutations(context.Background(), MutationFilter{TaskID: "not-an-id"})
	if err == nil {
		t.Fatal("a malformed task id must be rejected")
	}
}

// And the case: the same task, asked for in upper case, is the same history.
func TestListMutationsMatchesTheIDInAnyCase(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	s.RecordMutation(ctx, OpCreate, "0123456789abcdef01234567", "mcp", "")

	rows, _, err := s.ListMutations(ctx, MutationFilter{TaskID: "0123456789ABCDEF01234567"})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 — the same id in upper case is the same task", len(rows))
	}
}

// Validation is a pure rule, so it is testable without a database — and the
// store applies it BEFORE touching MongoDB, which is why a zero-value Store
// is enough to prove it.
func TestListMutationsRejectsUnknownOpWithoutTouchingMongo(t *testing.T) {
	var s Store
	_, _, err := s.ListMutations(context.Background(), MutationFilter{Ops: []string{"frobnicate"}})
	if err == nil {
		t.Fatal("unknown op must be rejected")
	}
}

func TestListMutationsReturnsNewestFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	s.RecordMutation(ctx, OpCreate, "aaaaaaaaaaaaaaaaaaaaaaaa", "mcp", "mcp create_task")
	s.RecordMutation(ctx, OpUpdate, "aaaaaaaaaaaaaaaaaaaaaaaa", "http", "name")
	s.RecordMutation(ctx, OpComplete, "aaaaaaaaaaaaaaaaaaaaaaaa", "mcp", "none")

	rows, truncated, err := s.ListMutations(ctx, MutationFilter{})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if truncated {
		t.Error("3 rows under the default limit must not be truncated")
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	// Newest first: complete, then update, then create.
	want := []string{OpComplete, OpUpdate, OpCreate}
	for i, op := range want {
		if rows[i].Op != op {
			t.Errorf("row %d op = %q, want %q", i, rows[i].Op, op)
		}
	}
	// Every entry carries the four facts the log exists for: what, which
	// task, who, when.
	if rows[0].TaskID != "aaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Errorf("task_id = %q, want the task it describes", rows[0].TaskID)
	}
	if rows[0].Source != "mcp" {
		t.Errorf("source = %q, want mcp", rows[0].Source)
	}
	if rows[0].At.IsZero() {
		t.Error("at must be set")
	}
}

func TestListMutationsFiltersByTaskAndOp(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	s.RecordMutation(ctx, OpCreate, "aaaaaaaaaaaaaaaaaaaaaaaa", "mcp", "")
	s.RecordMutation(ctx, OpUpdate, "aaaaaaaaaaaaaaaaaaaaaaaa", "http", "name")
	s.RecordMutation(ctx, OpCreate, "bbbbbbbbbbbbbbbbbbbbbbbb", "mcp", "")

	rows, _, err := s.ListMutations(ctx, MutationFilter{TaskID: "aaaaaaaaaaaaaaaaaaaaaaaa"})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("one task's rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.TaskID != "aaaaaaaaaaaaaaaaaaaaaaaa" {
			t.Errorf("row for %q leaked into the filter", r.TaskID)
		}
	}

	rows, _, err = s.ListMutations(ctx, MutationFilter{Ops: []string{OpCreate}})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("create rows = %d, want 2", len(rows))
	}

	rows, _, err = s.ListMutations(ctx, MutationFilter{TaskID: "aaaaaaaaaaaaaaaaaaaaaaaa", Ops: []string{OpCreate}})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("task-a create rows = %d, want 1", len(rows))
	}

	// The op is matched case-insensitively all the way down: the stored
	// ops are lowercase, and the filter lowers its copy before asking.
	rows, _, err = s.ListMutations(ctx, MutationFilter{Ops: []string{"CREATE"}})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("uppercase create rows = %d, want 2 — the filter must match the stored lowercase op", len(rows))
	}
}

// `since` is INCLUSIVE: a caller that has seen an entry at time T and asks
// for "since T" means "from T onwards", and dropping that entry would make
// the caller miss the very change it was polling for.
func TestListMutationsSinceIsInclusive(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	s.RecordMutation(ctx, OpCreate, "aaaaaaaaaaaaaaaaaaaaaaaa", "mcp", "")
	// The two writes must land in DIFFERENT milliseconds: `at` is stored at
	// millisecond precision, and two entries sharing one timestamp would
	// make the lower bound below match both — the test would then measure
	// the clock, not the contract.
	time.Sleep(2 * time.Millisecond)
	s.RecordMutation(ctx, OpUpdate, "aaaaaaaaaaaaaaaaaaaaaaaa", "mcp", "name")

	all, _, err := s.ListMutations(ctx, MutationFilter{})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("rows = %d, want 2", len(all))
	}
	newer, older := all[0].At, all[1].At

	rows, _, err := s.ListMutations(ctx, MutationFilter{Since: older})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("since=older rows = %d, want 2 (inclusive)", len(rows))
	}

	rows, _, err = s.ListMutations(ctx, MutationFilter{Since: newer})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("since=newer rows = %d, want 1", len(rows))
	}
}

// The truncation flag is the same contract List has: a caller must be able to
// tell "exactly N" from "N of many" without a second query.
func TestListMutationsReportsTruncation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		s.RecordMutation(ctx, OpCreate, "aaaaaaaaaaaaaaaaaaaaaaaa", "mcp", "")
	}

	rows, truncated, err := s.ListMutations(ctx, MutationFilter{Limit: 2})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if !truncated {
		t.Error("limit 2 over 3 rows must report truncation")
	}

	rows, truncated, err = s.ListMutations(ctx, MutationFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if truncated {
		t.Error("limit 10 over 3 rows must not report truncation")
	}
}

// A limit above the ceiling is clamped, not rejected: a caller asking for a
// huge page gets the biggest page the store serves, and the truncation flag
// tells it there was more. A limit below zero is the DEFAULT, not an error.
func TestListMutationsClampsTheLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		s.RecordMutation(ctx, OpCreate, "aaaaaaaaaaaaaaaaaaaaaaaa", "mcp", "")
	}
	rows, _, err := s.ListMutations(ctx, MutationFilter{Limit: MaxLimit + 1000})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	rows, truncated, err := s.ListMutations(ctx, MutationFilter{Limit: -5})
	if err != nil {
		t.Fatalf("a negative limit must fall back to the default: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}
	if truncated {
		t.Error("three rows under the default limit must not be truncated")
	}
}

// The ops are lowercased at the store seam, so a caller that bypasses
// ParseOps is answered the same way: "CREATE" is the op the log holds.
func TestListMutationsAcceptsOpsInAnyCase(t *testing.T) {
	var s Store
	_, _, err := s.ListMutations(context.Background(), MutationFilter{Ops: []string{"CREATE"}})
	if err == nil {
		return // a connected store answered, which is the point
	}
	if strings.Contains(err.Error(), "unknown op") {
		t.Fatalf("an op in the wrong case must be accepted: %v", err)
	}
}

// A store that was never connected is a programmer error, and it must say
// so rather than panic on a nil pointer.
func TestListMutationsOnAnUnconnectedStore(t *testing.T) {
	var s Store
	_, _, err := s.ListMutations(context.Background(), MutationFilter{})
	if err == nil {
		t.Fatal("an unconnected store must say so")
	}
	if !strings.Contains(err.Error(), "not connected") {
		t.Errorf("the error must name the cause: %v", err)
	}
}

// The log outlives its subject: a completed task is deleted by the 3-day TTL,
// but its entries stay readable for the log's own 90-day window. That is the
// whole point of the reader, so it is asserted rather than assumed.
func TestListMutationsSurvivesTheTaskItDescribes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc, err := s.Create(ctx, "Pay rent", "bank transfer", "2026-10-01", "09:00", intP(15), customRep("monthly on the 1st"), boolP(false), "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := doc["id"].(string)
	s.RecordMutation(ctx, OpCreate, id, "mcp", "")
	s.RecordMutation(ctx, OpComplete, id, "mcp", "none")

	// The task goes away (here: deleted outright, as the TTL would).
	if _, err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, id); err == nil {
		t.Fatal("task should be gone")
	}

	rows, _, err := s.ListMutations(ctx, MutationFilter{TaskID: id})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 — the log must outlive the task", len(rows))
	}
	for _, r := range rows {
		if r.TaskID != id {
			t.Errorf("row for %q leaked into the %q filter", r.TaskID, id)
		}
	}
}

// The store returns an empty LIST, never a nil slice: the transports both
// promise `[]` on the wire, and a direct caller marshalling this slice must
// not get `null` for the same answer.
func TestListMutationsReturnsAnEmptyListNotNil(t *testing.T) {
	s := testStore(t)
	rows, truncated, err := s.ListMutations(context.Background(), MutationFilter{
		TaskID: "aaaaaaaaaaaaaaaaaaaaaaaa",
	})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if rows == nil {
		t.Fatal("rows must be an empty list, not nil")
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
	if truncated {
		t.Error("an empty result must not be truncated")
	}
}

// The write side normalizes too. The verbs accept an UPPERCASE hex id —
// ObjectIDFromHex does — so without this a row would be stored under an
// upper-case `task_id` that the lowercased read filter can never match: the
// task was touched, and the history says it never was.
func TestRecordMutationNormalizesTheTaskID(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	s.RecordMutation(ctx, OpCreate, "0123456789ABCDEF01234567", "mcp", "")

	rows, _, err := s.ListMutations(ctx, MutationFilter{TaskID: "0123456789abcdef01234567"})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 — the row must be findable", len(rows))
	}
	if rows[0].TaskID != "0123456789abcdef01234567" {
		t.Errorf("task_id = %q, want it stored lowercase", rows[0].TaskID)
	}
}

// A filter that was sent and names nothing is refused at the seam too, so
// a direct caller is not handed the whole log for an empty filter.
func TestListMutationsRejectsAnOpFilterThatNamesNothing(t *testing.T) {
	var s Store
	_, _, err := s.ListMutations(context.Background(), MutationFilter{Ops: []string{"", " "}})
	if err == nil {
		t.Fatal("an op filter that names nothing must be rejected, not read as the whole log")
	}
}

// The timestamps are stored and returned in UTC, so a log read from two
// machines does not disagree about when an entry happened.
func TestListMutationsTimestampsAreUTC(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	s.RecordMutation(ctx, OpCreate, "aaaaaaaaaaaaaaaaaaaaaaaa", "mcp", "")
	rows, _, err := s.ListMutations(ctx, MutationFilter{})
	if err != nil {
		t.Fatalf("ListMutations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if _, offset := rows[0].At.Zone(); offset != 0 {
		t.Errorf("at must be UTC, got offset %d", offset)
	}
	if time.Since(rows[0].At) > time.Minute {
		t.Errorf("at = %v, want roughly now", rows[0].At)
	}
}
