package memory

import (
	"context"
	"strings"
	"testing"
)

func TestNormalizeTopic(t *testing.T) {
	cases := map[string]string{
		"Work Style":    "work-style",
		"  health  ":    "health",
		"work-style":    "work-style",
		"":              DefaultTopic,
		"---":           DefaultTopic,
		"Relationships": "relationships",
	}
	for in, want := range cases {
		if got := NormalizeTopic(in); got != want {
			t.Errorf("NormalizeTopic(%q) = %q, want %q", in, got, want)
		}
	}
	if got := NormalizeTopic(strings.Repeat("a", MaxTopic+10)); len(got) != MaxTopic {
		t.Errorf("long topic = %d runes, want %d", len(got), MaxTopic)
	}
}

func TestCleanTextBounds(t *testing.T) {
	if got := CleanText("  hi  "); got != "hi" {
		t.Fatalf("CleanText trim = %q", got)
	}
	if got := CleanText(strings.Repeat("é", MaxText+5)); len([]rune(got)) != MaxText {
		t.Fatalf("CleanText bound = %d runes, want %d", len([]rune(got)), MaxText)
	}
}

func TestPromptBlockOnlyConfirmedUnpaused(t *testing.T) {
	items := []Item{
		{Topic: "style", Text: "Prefers tables for weekly reports.", Status: StatusConfirmed},
		{Topic: "health", Text: "Training for a half marathon.", Status: StatusConfirmed},
		{Topic: "fact", Text: "Proposed but not confirmed.", Status: StatusPending},
	}
	got := PromptBlock(items, []string{"health"})
	if strings.Contains(got, "half marathon") {
		t.Fatalf("paused topic leaked into prompt: %q", got)
	}
	if strings.Contains(got, "not confirmed") {
		t.Fatalf("pending item leaked into prompt: %q", got)
	}
	if !strings.Contains(got, "(style) Prefers tables") {
		t.Fatalf("confirmed item missing: %q", got)
	}
	if !strings.HasPrefix(got, "[Memory:") || !strings.HasSuffix(got, "[/Memory]") {
		t.Fatalf("block is not delimited: %q", got)
	}
}

func TestPromptBlockCapsAndTerminator(t *testing.T) {
	if got := PromptBlock(nil, nil); got != "" {
		t.Fatalf("empty block = %q", got)
	}
	items := make([]Item, 0, PromptItems+10)
	for i := 0; i < PromptItems+10; i++ {
		items = append(items, Item{
			Topic:  "general",
			Text:   strings.Repeat("x", 20),
			Status: StatusConfirmed,
		})
	}
	got := PromptBlock(items, nil)
	if strings.Count(got, "\n- ") != PromptItems {
		t.Fatalf("items in block = %d, want %d", strings.Count(got, "\n- "), PromptItems)
	}
	if !strings.HasSuffix(got, "[/Memory]") {
		t.Fatalf("block lost its terminator: %q", got[len(got)-40:])
	}
	if len(got) > PromptBytes {
		t.Fatalf("block = %d bytes, cap %d", len(got), PromptBytes)
	}

	// Skip-and-continue: one oversized item must not starve older items that
	// fit. A bare header would be worse than none, so all-oversized is still
	// "".
	// Multi-byte text: MaxText runes can still exceed the byte budget.
	big := Item{Topic: "general", Text: strings.Repeat("記", MaxText), Status: StatusConfirmed}
	if got := PromptBlock([]Item{big}, nil); got != "" {
		t.Fatalf("oversized block = %q", got)
	}
	small := Item{Topic: "general", Text: "short", Status: StatusConfirmed}
	if got := PromptBlock([]Item{big, small}, nil); !strings.Contains(got, "short") {
		t.Fatalf("fittable item starved by an oversized one: %q", got)
	}
}

func TestMemoryStoreCRUD(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	a, err := s.Create(ctx, Item{Profile: "p", Topic: "Work Style", Text: "uses tables", Source: SourceUser, Status: StatusConfirmed})
	if err != nil {
		t.Fatal(err)
	}
	if a.Topic != "work-style" || a.Status != StatusConfirmed || a.ID == "" {
		t.Fatalf("created %+v", a)
	}
	b, err := s.Create(ctx, Item{Profile: "p", Topic: "fact", Text: "has a dog", Source: SourceAgent, Status: StatusPending})
	if err != nil {
		t.Fatal(err)
	}

	list, err := s.List(ctx, Filter{Profile: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != b.ID {
		t.Fatalf("list order = %+v", list)
	}
	if pending, _ := s.List(ctx, Filter{Profile: "p", Status: StatusPending}); len(pending) != 1 {
		t.Fatalf("pending filter = %+v", pending)
	}
	if other, _ := s.List(ctx, Filter{Profile: "q"}); len(other) != 0 {
		t.Fatalf("profile leak = %+v", other)
	}

	status := StatusConfirmed
	text := "has two dogs"
	if _, err := s.Update(ctx, b.ID, "p", Update{Text: &text, Status: &status}); err != nil {
		t.Fatal(err)
	}
	// A foreign profile's id is NotFound, and the row is untouched.
	other := "someone else"
	if _, err := s.Update(ctx, b.ID, "q", Update{Text: &other}); err == nil {
		t.Fatal("foreign update should be NotFound")
	}
	if err := s.Delete(ctx, b.ID, "q"); err == nil {
		t.Fatal("foreign delete should be NotFound")
	}
	if got, _ := s.Get(ctx, b.ID); got.Text != text {
		t.Fatalf("foreign write changed the row: %+v", got)
	}
	// Create enforces the text invariant, not just the callers.
	if _, err := s.Create(ctx, Item{Profile: "p", Text: "   "}); err == nil {
		t.Fatal("empty-text create should fail")
	}
	long, err := s.Create(ctx, Item{Profile: "p", Text: strings.Repeat("y", MaxText+10)})
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(long.Text)); n != MaxText {
		t.Fatalf("over-long create stored %d runes, want clipped to %d", n, MaxText)
	}
	if err := s.Delete(ctx, long.ID, "p"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get(ctx, b.ID)
	if got.Text != text || got.Status != StatusConfirmed {
		t.Fatalf("update = %+v", got)
	}

	if err := s.SetTopicPaused(ctx, "p", "health", true); err != nil {
		t.Fatal(err)
	}
	if paused, _ := s.PausedTopics(ctx, "p"); len(paused) != 1 || paused[0] != "health" {
		t.Fatalf("paused = %+v", paused)
	}
	if err := s.SetTopicPaused(ctx, "p", "health", false); err != nil {
		t.Fatal(err)
	}
	if paused, _ := s.PausedTopics(ctx, "p"); len(paused) != 0 {
		t.Fatalf("resume left %+v", paused)
	}

	if n, err := s.DeleteAll(ctx, "p"); err != nil || n != 2 {
		t.Fatalf("forget = %d, %v", n, err)
	}
	if err := s.Delete(ctx, a.ID, "p"); err == nil {
		t.Fatal("deleting a forgotten item should be NotFound")
	}
	// PausedTopics is a list, never null: the screen decodes it as an array.
	if paused, _ := s.PausedTopics(ctx, "nobody"); paused == nil {
		t.Fatal("fresh-profile paused topics encoded as null")
	}
}
