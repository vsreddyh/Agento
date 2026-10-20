package approvals

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestStatusTerminal(t *testing.T) {
	for _, s := range []Status{StatusApproved, StatusDenied, StatusExpired} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
		if !s.Valid() {
			t.Errorf("%s should be valid", s)
		}
	}
	if StatusPending.Terminal() {
		t.Error("pending must not be terminal")
	}
}

func TestPolicyGatesMoneyAndDeletes(t *testing.T) {
	gated := [][2]string{
		{"miser-money", "log_transaction"},
		{"miser-money", "delete_transactions"},
		{"miser-money", "prune_old"},
		{"cookbook", "delete_recipe"},
		{"health-check", "delete_meals"},
		{"task-manager", "delete_task"},
		{"project-manager", "delete_project"},
		{"notes", "delete_note"},
		{"miser-money", "brand_new_tool"},
		{"unknown-server", "list_things"},
	}
	for _, tc := range gated {
		if !NeedsApproval(tc[0], tc[1], nil) {
			t.Errorf("%s/%s should gate", tc[0], tc[1])
		}
	}
	free := [][2]string{
		{"miser-money", "list_accounts"},
		{"miser-money", "query_transactions"},
		{"cookbook", "list_recipes"},
		{"health-check", "log_meal"},
		{"task-manager", "create_task"},
		{"task-manager", "complete_task"},
		{"notes", "create_note"},
	}
	for _, tc := range free {
		if NeedsApproval(tc[0], tc[1], nil) {
			t.Errorf("%s/%s should run free", tc[0], tc[1])
		}
	}
	// A counting prune changes nothing.
	if NeedsApproval("miser-money", "prune_old", map[string]any{"dry_run": true}) {
		t.Error("dry-run prune should run free")
	}
	// Missing dry_run fails closed.
	if !NeedsApproval("miser-money", "prune_old", map[string]any{}) {
		t.Error("prune without dry_run should gate")
	}
}

// TestPolicyTableIsComplete pins the fail-closed contract: every tool the
// servers publish must have an explicit rule. The `known` map is hand-written
// on purpose — there is no shared registry the server binaries export, and
// generating it would mean refactoring every main for a test.
//
// That makes the maintenance rule part of the contract: a PR that adds or
// renames a tool updates `known` HERE in the same change, exactly as it
// updates gatedTools. Without both, the test only checks the two hardcoded
// copies against each other. Runtime stays safe regardless (an unknown tool on
// a known server holds), so forgetting yields a test that under-reports, never
// an ungated write.
func TestPolicyTableIsComplete(t *testing.T) {
	known := map[string][]string{
		"miser-money": {"create_account", "list_accounts", "archive_account",
			"get_balances", "log_transaction", "log_text", "query_transactions",
			"summarize", "fix_last_transaction", "delete_transactions", "prune_old"},
		"cookbook": {"add_ingredient", "list_ingredients", "delete_ingredient",
			"add_recipe", "get_recipe", "list_recipes", "update_recipe",
			"delete_recipe", "scale_recipe", "log_cook", "list_cooks"},
		"health-check": {"log_meal", "fix_last_meal", "query_meals",
			"delete_meals", "log_weight", "log_sleep", "log_workout",
			"daily_summary", "prune_old"},
		"task-manager": {"create_task", "list_tasks", "get_task", "update_task",
			"complete_task", "skip_task", "reopen_task", "delete_task"},
		"project-manager": {"create_project", "list_projects", "get_project",
			"update_project", "delete_project"},
		"notes": {"create_note", "get_note", "list_notes", "search_notes",
			"update_note", "delete_note"},
	}
	n := 0
	for server, tools := range known {
		rules, ok := gatedTools[server]
		if !ok {
			t.Fatalf("server %s has no policy table", server)
		}
		for _, tool := range tools {
			if _, ok := rules[tool]; !ok {
				t.Errorf("tool %s/%s has no explicit rule", server, tool)
			}
			n++
		}
		if len(rules) != len(tools) {
			t.Errorf("server %s table has %d rules for %d tools", server, len(rules), len(tools))
		}
	}
	if n != 50 {
		t.Errorf("pinned %d tools, want the 50 inventoried", n)
	}
}

func TestRedactMasksSecrets(t *testing.T) {
	in := map[string]any{
		"amount": 5, "api_token": "sekret", "Note": "x",
		"params": map[string]any{"passwd": "hunter2", "limit": 3},
		"tags":   []any{"a", map[string]any{"pwd": "y"}},
	}
	out := Redact(in)
	if out["api_token"] != "***" {
		t.Errorf("token not masked: %v", out)
	}
	nested, _ := out["params"].(map[string]any)
	if nested["passwd"] != "***" || nested["limit"] != 3 {
		t.Errorf("nested not masked: %v", out)
	}
	tags, _ := out["tags"].([]any)
	if m, _ := tags[1].(map[string]any); m["pwd"] != "***" {
		t.Errorf("array-nested not masked: %v", out)
	}
	if out["amount"] != 5 || in["api_token"] != "sekret" {
		t.Errorf("redact mutated input or dropped fields: %v", out)
	}
	if len(Redact(nil)) != 0 {
		t.Error("nil args should give an empty map")
	}
}

func TestMemoryApprovalLifecycle(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	a := Approval{
		ID: NewID(), Server: "miser-money", Tool: "log_transaction",
		Args: map[string]any{"amount": 5},
	}
	if err := m.CreateApproval(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetApproval(ctx, a.ID)
	if err != nil || got.Status != StatusPending {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	decided, err := m.DecideApproval(ctx, a.ID, false, "app", time.Now())
	if err != nil || decided.Status != StatusDenied {
		t.Fatalf("Decide = %+v, %v", decided, err)
	}
	// A decision lands exactly once.
	if _, err := m.DecideApproval(ctx, a.ID, true, "app", time.Now()); err == nil {
		t.Fatal("second decision should fail")
	} else if se, ok := err.(*StoreError); !ok || !se.Conflict {
		t.Fatalf("second decision should conflict, got %v", err)
	}
	// Timeout on a decided row is a no-op.
	if err := m.TimeoutApproval(ctx, a.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	list, err := m.ListApprovals(ctx, ApprovalFilter{Status: StatusDenied})
	if err != nil || len(list) != 1 {
		t.Fatalf("list denied = %d, %v", len(list), err)
	}
}

func TestMemoryTimeoutAndExpiry(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	now := time.Now()
	m.now = func() time.Time { return now }
	a := Approval{ID: NewID(), Server: "notes", Tool: "delete_note"}
	if err := m.CreateApproval(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := m.TimeoutApproval(ctx, a.ID, now); err != nil {
		t.Fatal(err)
	}
	got, err := m.GetApproval(ctx, a.ID)
	if err != nil || got.Status != StatusExpired {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	// Past expiry the row reads gone.
	m.now = func() time.Time { return now.Add(ApprovalTTL + time.Minute) }
	if _, err := m.GetApproval(ctx, a.ID); err == nil {
		t.Fatal("expired row should read NotFound")
	} else if se, ok := err.(*StoreError); !ok || !se.NotFound {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestMemoryMarkInterrupted(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	now := time.Now()
	pending := Approval{ID: NewID(), Server: "notes", Tool: "delete_note"}
	old := Approval{ID: NewID(), Server: "notes", Tool: "delete_note",
		ExpiresAt: now.Add(-time.Minute)}
	for _, a := range []Approval{pending, old} {
		if err := m.CreateApproval(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	// at is the restart instant: the rows above were created before it, so the
	// same scope Mongo uses (createdAt < at) must still expire them.
	n, err := m.MarkInterrupted(ctx, now.Add(time.Second))
	if err != nil || n != 1 {
		t.Fatalf("marked %d, err %v", n, err)
	}
	got, err := m.GetApproval(ctx, pending.ID)
	if err != nil || got.Status != StatusExpired {
		t.Fatalf("pending row = %+v, %v", got, err)
	}
	if _, err := m.GetApproval(ctx, old.ID); err == nil {
		t.Fatal("expired row should be deleted, not flipped")
	}

	// A row created after the restart instant belongs to this process and
	// must survive the sweep.
	m2 := NewMemoryStore()
	late := Approval{ID: NewID(), Server: "notes", Tool: "delete_note"}
	if err := m2.CreateApproval(ctx, late); err != nil {
		t.Fatal(err)
	}
	if n, err := m2.MarkInterrupted(ctx, time.Now().Add(-time.Hour)); err != nil || n != 0 {
		t.Fatalf("marked %d late rows, err %v", n, err)
	}
	if got, err := m2.GetApproval(ctx, late.ID); err != nil || got.Status != StatusPending {
		t.Fatalf("late row = %+v, %v; want still pending", got, err)
	}
}

func TestMemoryAuditCap(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	for i := 0; i < 10*ListCap+3; i++ {
		if err := m.Audit(ctx, AuditEntry{
			ID: NewID(), Server: "notes", Tool: "list_notes",
			Disposition: DispositionAllow, At: int64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := m.ListAudit(ctx, AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	// List itself caps at ListCap: assert the window holds the newest rows,
	// which proves the oldest were evicted rather than kept.
	if len(list) != ListCap {
		t.Fatalf("audit list holds %d rows, want %d", len(list), ListCap)
	}
	for _, e := range list {
		if e.At < 903 {
			t.Fatalf("evicted row survived: at=%d", e.At)
		}
	}
}

// Audit errors are free text from a server, so they need a value-shape scrub
// as well as the key-based redaction used for arguments.
func TestScrubErrorMasksCredentials(t *testing.T) {
	cases := []struct{ in, want string }{
		{"invalid token sk-live-abcdef123456", "[redacted]"},
		{"Authorization: Bearer abcdefghijklmnop", "[redacted]"},
		{"api_key=supersecretvalue failed", "[redacted]"},
		{`{"token":"sk-abcdef123456"}`, "[redacted]"},
		// Compact JSON: the match must stop at the value, not swallow the
		// following fields into the replacement.
		{`{"token":"sk-abcdef123456","note":"keep"}`, "[redacted]"},
		{"passwd=hunter2 failed", "[redacted]"},
		{`token="correct horse battery staple"`, "[redacted]"},
		{"no such note: abc", "no such note: abc"},
	}
	for _, c := range cases {
		got := ScrubError(c.in)
		if c.want == "[redacted]" {
			if strings.Contains(got, "sk-live") || strings.Contains(got, "abcdefghijklmnop") ||
				strings.Contains(got, "supersecretvalue") {
				t.Fatalf("ScrubError(%q) = %q, credential survived", c.in, got)
			}
			if !strings.Contains(got, "[redacted]") {
				t.Fatalf("ScrubError(%q) = %q, want a redaction marker", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Fatalf("ScrubError(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Extended key set and space-containing quoted values.
	if got := ScrubError("passwd=hunter2 failed"); strings.Contains(got, "hunter2") {
		t.Fatalf("passwd value survived: %q", got)
	}
	if got := ScrubError(`token="correct horse battery staple"`); strings.Contains(got, "correct horse") {
		t.Fatalf("quoted multi-word value survived: %q", got)
	}

	// Compact JSON: the value match must terminate, leaving the next field.
	got := ScrubError(`{"token":"sk-abcdef123456","note":"keep"}`)
	if strings.Contains(got, "sk-abcdef") || !strings.Contains(got, `"note":"keep"`) {
		t.Fatalf("compact JSON scrub = %q, want the secret masked and note kept", got)
	}

	long := strings.Repeat("x", MaxAuditError+50)
	if n := utf8.RuneCountInString(ScrubError(long)); n != MaxAuditError {
		t.Fatalf("long error scrubbed to %d runes, want %d", n, MaxAuditError)
	}
}

// The memory audit honours the same 90-day retention contract as Mongo: a row
// past ExpiresAt stops listing even though no TTL monitor exists here.
func TestMemoryAuditExpiry(t *testing.T) {
	store := NewMemoryStore()
	now := time.Now()
	rows := []AuditEntry{
		{ID: "live", Server: "notes", Tool: "get_note", At: now.UnixMilli()},
		{ID: "dead", Server: "notes", Tool: "get_note", At: now.UnixMilli() - 1000,
			ExpiresAt: now.Add(-time.Second)},
	}
	for _, e := range rows {
		if err := store.Audit(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.ListAudit(context.Background(), AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "live" {
		t.Fatalf("audit = %+v, want only the live row", got)
	}
	if got[0].ExpiresAt.IsZero() {
		t.Fatal("memory audit row kept a zero ExpiresAt; retention is unspecified")
	}
}

// KnownServer is what the relay warns on: a typo must be detectable without
// importing the unexported table.
func TestKnownServer(t *testing.T) {
	if !KnownServer("notes") || !KnownServer("miser-money") {
		t.Fatal("a wired server is not in the policy table")
	}
	if KnownServer("miser_monet") || KnownServer("") {
		t.Fatal("an unwired server name reported as known")
	}
}

// The store redacts on write, not only the call sites: one caller that forgets
// must not be able to persist a secret for the retention window.
func TestStoresRedactOnWrite(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()
	if err := store.CreateApproval(ctx, Approval{
		ID: "a1", Server: "notes", Tool: "get_note", Status: StatusPending,
		Args: map[string]any{"password": "hunter2", "id": "n1"},
	}); err != nil {
		t.Fatal(err)
	}
	a, err := store.GetApproval(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if a.Args["password"] != "***" || a.Args["id"] != "n1" {
		t.Fatalf("approval args = %#v, want password masked and id intact", a.Args)
	}
	if err := store.Audit(ctx, AuditEntry{
		ID: "e1", Server: "notes", Tool: "get_note", Disposition: DispositionAllow,
		Args: map[string]any{"token": "sk-live-abc"},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.ListAudit(ctx, AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Args["token"] != "***" {
		t.Fatalf("audit args = %#v, want token masked", rows)
	}
	if rows[0].At == 0 || rows[0].ExpiresAt.IsZero() {
		t.Fatalf("audit stamps = At %d / ExpiresAt %v, want both stamped by the store",
			rows[0].At, rows[0].ExpiresAt)
	}
}

// A relay restart expires only its own server's queue: six relays share one
// collection, and one restarting must not kill another's live hold.
func TestMarkServerInterruptedIsScoped(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	mine := Approval{ID: NewID(), Server: "notes", Tool: "delete_note"}
	other := Approval{ID: NewID(), Server: "miser-money", Tool: "log_transaction"}
	for _, a := range []Approval{mine, other} {
		if err := m.CreateApproval(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	n, err := m.MarkServerInterrupted(ctx, "notes", time.Now())
	if err != nil || n != 1 {
		t.Fatalf("marked %d, err %v; want 1", n, err)
	}
	if got, err := m.GetApproval(ctx, mine.ID); err != nil || got.Status != StatusExpired {
		t.Fatalf("own row = %+v, %v; want expired", got, err)
	}
	if got, err := m.GetApproval(ctx, other.ID); err != nil || got.Status != StatusPending {
		t.Fatalf("other server row = %+v, %v; want untouched pending", got, err)
	}
}
