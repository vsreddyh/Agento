package runs

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestStatusTerminal(t *testing.T) {
	terminal := []Status{StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted}
	for _, s := range terminal {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	if StatusRunning.Terminal() {
		t.Error("running must not be terminal")
	}
	if Status("bogus").Terminal() {
		t.Error("an unknown status must not be treated as finished")
	}
}

func TestCapTextKeepsValidUTF8(t *testing.T) {
	// Cut lands inside the three-byte é (0xC3 0xA9 is two bytes; use a
	// three-byte rune so a naive byte slice would split it).
	s := "aaaa€bbb"
	got := CapText(s, 6) // 4 'a' + first byte of € at index 5
	if !strings.HasSuffix(got, "… (truncated)") {
		t.Fatalf("truncated text should say so: %q", got)
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("truncation split a rune: %q", got)
	}
	if !strings.HasPrefix(got, "aaaa") {
		t.Fatalf("kept the wrong prefix: %q", got)
	}
}

func TestCapTextLeavesShortTextAlone(t *testing.T) {
	if got := CapText("short", 100); got != "short" {
		t.Fatalf("got %q", got)
	}
}

func TestCapTextIsIdempotent(t *testing.T) {
	// A prompt trimmed by TrimPrompt and then stored through Capped passes
	// through CapText twice; the second pass must not cut and mark again.
	once := CapText(strings.Repeat("x", 5000), MaxPrompt)
	twice := CapText(once, MaxPrompt)
	if twice != once {
		t.Fatalf("second cap changed the value: %d bytes vs %d", len(twice), len(once))
	}
	if n := strings.Count(twice, "… (truncated)"); n != 1 {
		t.Fatalf("marker appears %d times, want 1", n)
	}
}

func TestSaveNeedsAnID(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	if err := m.Save(ctx, Run{Status: StatusRunning}); err == nil {
		t.Fatal("Save with no id should fail, not store under an empty key")
	}
}

func TestMemoryStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	now := time.Now()
	r := Run{
		ID: "r1", Profile: "chat", ConversationID: "c1", Prompt: "hello",
		Status: StatusRunning, CreatedAt: now.UnixMilli(), ExpiresAt: now.Add(DefaultTTL),
	}
	if err := m.Create(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := m.Get(ctx, "r1")
	if err != nil || got.Prompt != "hello" || got.Status != StatusRunning {
		t.Fatalf("Get = %+v, %v", got, err)
	}
	got.Output = "partial"
	got.Status = StatusCompleted
	got.FinishedAt = now.UnixMilli()
	if err := m.Save(ctx, got); err != nil {
		t.Fatal(err)
	}
	if again, _ := m.Get(ctx, "r1"); again.Output != "partial" || again.Status != StatusCompleted {
		t.Fatalf("save did not replace: %+v", again)
	}
	list, err := m.List(ctx, Filter{ConversationID: "c1"})
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if _, err := m.Get(ctx, "missing"); !NotFound(err) {
		t.Fatalf("missing run error = %v, want NotFound", err)
	}
}

func TestMemoryStoreListOrderCapAndFilters(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	for i := 0; i < 5; i++ {
		_ = m.Create(ctx, Run{
			ID:     string(rune('a' + i)),
			Status: StatusRunning,
			// Newest first: createdAt grows with i.
			CreatedAt: int64(i + 1),
		})
	}
	list, _ := m.List(ctx, Filter{Limit: 3})
	if len(list) != 3 {
		t.Fatalf("limit: got %d", len(list))
	}
	if list[0].ID != "e" {
		t.Fatalf("newest first, got %q", list[0].ID)
	}
	_ = m.Save(ctx, Run{ID: "a", Status: StatusCompleted, CreatedAt: 1})
	done, _ := m.List(ctx, Filter{Status: StatusCompleted})
	if len(done) != 1 || done[0].ID != "a" {
		t.Fatalf("status filter = %+v", done)
	}
}

func TestMarkInterrupted(t *testing.T) {
	ctx := context.Background()
	m := NewMemoryStore()
	_ = m.Create(ctx, Run{ID: "live", Status: StatusRunning})
	_ = m.Create(ctx, Run{ID: "done", Status: StatusCompleted})
	n, err := m.MarkInterrupted(ctx, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("marked %d, err %v", n, err)
	}
	live, _ := m.Get(ctx, "live")
	if live.Status != StatusInterrupted || live.Error == "" || live.FinishedAt == 0 {
		t.Fatalf("live run = %+v", live)
	}
	done, _ := m.Get(ctx, "done")
	if done.Status != StatusCompleted {
		t.Fatalf("completed run was touched: %+v", done)
	}
}

func TestCappedBoundsStoredFields(t *testing.T) {
	r := Run{
		Prompt:    strings.Repeat("p", MaxPrompt+10),
		Output:    strings.Repeat("o", MaxOutput+10),
		Reasoning: strings.Repeat("r", MaxReasoning+10),
		Tools:     make([]string, MaxTools+10),
	}.Capped()
	if len([]rune(r.Prompt)) > MaxPrompt+16 {
		t.Error("prompt not bounded")
	}
	if !strings.HasSuffix(r.Output, "… (truncated)") {
		t.Error("output not bounded")
	}
	if len(r.Tools) != MaxTools {
		t.Errorf("tools = %d, want %d", len(r.Tools), MaxTools)
	}
}
