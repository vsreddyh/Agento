package cronjobs

import (
	"context"
	"os"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		t.Skip("MONGODB_URI not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := New(uri, "cron_test")
	if err != nil {
		t.Skipf("mongo unreachable: %v", err)
	}
	if err := s.jobs.Drop(ctx); err != nil {
		t.Fatalf("drop: %v", err)
	}
	return s
}

func strP(s string) *string { return &s }
func boolP(b bool) *bool    { return &b }

func TestStoreCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	if _, err := s.Create(ctx, "n", "", "0 9 * * *", ""); err == nil {
		t.Fatal("empty prompt should fail")
	}
	if _, err := s.Create(ctx, "n", "p", "nope", ""); err == nil {
		t.Fatal("bad schedule should fail")
	}
	j, err := s.Create(ctx, "standup", "summarize", "0 9 * * *", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if j.ID == "" || j.Delivery != "local" || j.Paused || len(j.Runs) != 0 {
		t.Fatalf("bad create: %+v", j)
	}

	got, err := s.Get(ctx, j.ID)
	if err != nil || got.Prompt != "summarize" {
		t.Fatalf("get: %+v %v", got, err)
	}
	if _, err := s.Get(ctx, "nope"); err == nil {
		t.Fatal("bad id should fail")
	}
	if _, err := s.Get(ctx, "0123456789abcdef01234567"); err == nil {
		t.Fatal("unknown id should fail")
	}

	list, err := s.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %+v %v", list, err)
	}

	paused := true
	upd, err := s.Update(ctx, j.ID, nil, nil, nil, nil, &paused)
	if err != nil || !upd.Paused {
		t.Fatalf("pause: %+v %v", upd, err)
	}
	if _, err := s.Update(ctx, j.ID, nil, nil, strP("bad"), nil, nil); err == nil {
		t.Fatal("bad schedule update should fail")
	}
	name := "renamed"
	upd, err = s.Update(ctx, j.ID, &name, nil, nil, nil, boolP(false))
	if err != nil || upd.Name != "renamed" || upd.Paused {
		t.Fatalf("update: %+v %v", upd, err)
	}

	// History caps at 10, newest last.
	for i := 0; i < 12; i++ {
		if err := s.RecordRun(ctx, j.ID,
			Run{At: int64(i), Text: "r"}, int64(i)); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	got, _ = s.Get(ctx, j.ID)
	if len(got.Runs) != RunHistoryCap || got.Runs[0].Text != "r" || got.LastFire != 11 {
		t.Fatalf("cap: %d runs, lastFire %d", len(got.Runs), got.LastFire)
	}

	if err := s.Delete(ctx, j.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Delete(ctx, j.ID); err == nil {
		t.Fatal("double delete should fail")
	}
}
