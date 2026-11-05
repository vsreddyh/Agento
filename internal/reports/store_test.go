package reports

import (
	"context"
	"os"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("MONGODB_URI") == "" {
		t.Skip("MONGODB_URI not set")
	}
	t.Setenv("MONGODB_URI", os.Getenv("MONGODB_URI"))
	if db := os.Getenv("MONGODB_DB"); db != "" {
		t.Setenv("MONGODB_DB", db)
	}
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestCreateGetRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	created, err := s.CreateReport(ctx, "Q3 findings", "Body with $x^2$ math.", "t3-agent")
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}
	id, _ := created["_id"].(string)
	if id == "" {
		t.Fatalf("no id: %+v", created)
	}
	for _, k := range []string{"title", "body_md", "source", "createdAt"} {
		if created[k] == nil || created[k] == "" {
			t.Fatalf("missing %s: %+v", k, created)
		}
	}
	if created["read"] != false || created["starred"] != false {
		t.Fatalf("new report must start unread+unstarred: %+v", created)
	}
	got, err := s.GetReport(ctx, id)
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if got == nil || got["body_md"] != "Body with $x^2$ math." {
		t.Fatalf("got %+v", got)
	}
	t.Cleanup(func() {
		_, _ = s.DeleteReport(context.Background(), id)
	})
}

func TestGetMissingIsNil(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	got, err := s.GetReport(ctx, "000000000000000000000000")
	if err != nil || got != nil {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := s.GetReport(ctx, "junk id !!!"); err == nil {
		t.Fatal("bad id should error")
	}
}

func TestListNewestFirst(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var ids []string
	for _, title := range []string{"first", "second"} {
		c, err := s.CreateReport(ctx, title, "line one\nline two", "probe")
		if err != nil {
			t.Fatalf("CreateReport: %v", err)
		}
		id, _ := c["_id"].(string)
		ids = append(ids, id)
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = s.DeleteReport(context.Background(), id)
		}
	})
	rows, err := s.ListReports(ctx, 0)
	if err != nil {
		t.Fatalf("ListReports: %v", err)
	}
	if len(rows) < 2 || rows[0]["title"] != "second" {
		t.Fatalf("not newest-first: %+v", rows)
	}
	if _, ok := rows[0]["body_md"]; ok {
		t.Fatal("list must return summaries, not bodies")
	}
	if rows[0]["preview"] != "line one" {
		t.Fatalf("preview = %v", rows[0]["preview"])
	}
}

func TestUpdateReadStarred(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := s.CreateReport(ctx, "patch me", "body", "probe")
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}
	id, _ := c["_id"].(string)
	t.Cleanup(func() {
		_, _ = s.DeleteReport(context.Background(), id)
	})
	upd, err := s.UpdateReport(ctx, id, map[string]any{"read": true})
	if err != nil {
		t.Fatalf("UpdateReport: %v", err)
	}
	if upd["read"] != true || upd["starred"] != false {
		t.Fatalf("got %+v", upd)
	}
	if _, err := s.UpdateReport(ctx, id, map[string]any{"title": "nope"}); err == nil {
		t.Fatal("content edit should be refused")
	}
	if _, err := s.UpdateReport(ctx, id, map[string]any{"read": "yes"}); err == nil {
		t.Fatal("non-bool read should be refused")
	}
	missing, err := s.UpdateReport(ctx, "000000000000000000000000", map[string]any{"read": true})
	if err != nil || missing != nil {
		t.Fatalf("got %+v, %v", missing, err)
	}
}

func TestDeleteIdempotent(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := s.CreateReport(ctx, "delete me", "body", "probe")
	if err != nil {
		t.Fatalf("CreateReport: %v", err)
	}
	id, _ := c["_id"].(string)
	done, err := s.DeleteReport(ctx, id)
	if err != nil || !done {
		t.Fatalf("got %v, %v", done, err)
	}
	done, err = s.DeleteReport(ctx, id)
	if err != nil || done {
		t.Fatalf("second delete should report nothing: %v, %v", done, err)
	}
}
