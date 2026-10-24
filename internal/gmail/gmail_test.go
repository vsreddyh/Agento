package gmail

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

// Stub Gmail wire: list/get/draft shapes only, no auth.
func stubGmail(t *testing.T) *httptest.Server {
	t.Helper()
	body := base64.URLEncoding.EncodeToString([]byte("hello world"))
	long := strings.Repeat("x", MaxBody+10)
	longEnc := base64.URLEncoding.EncodeToString([]byte(long))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages"):
			fmt.Fprint(w, `{"messages":[{"id":"m1","threadId":"t1"}],"resultSizeEstimate":1}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages/m1"):
			fmt.Fprintf(w, `{"id":"m1","threadId":"t1","snippet":"hello","payload":{`+
				`"mimeType":"multipart/alternative","headers":[`+
				`{"name":"Subject","value":"Hi"},{"name":"From","value":"boss@example.com"},{"name":"Date","value":"Mon, 26 Oct 2026 09:00:00 +0000"}],`+
				`"parts":[{"mimeType":"text/plain","body":{"data":%q}}]}}`, body)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages/mlong"):
			fmt.Fprintf(w, `{"id":"mlong","threadId":"t1","snippet":"x","payload":{`+
				`"mimeType":"text/plain","headers":[],"body":{"data":%q}}}`, longEnc)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/users/me/drafts"):
			fmt.Fprint(w, `{"id":"d1","message":{"id":"m9"}}`)
		default:
			t.Errorf("unexpected gmail call %s %s", r.Method, r.URL.Path)
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
}

func testClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"})
	c, err := NewClientWithToken(context.Background(), src, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestListReturnsHeadersWithoutBodies(t *testing.T) {
	srv := stubGmail(t)
	defer srv.Close()
	rows, err := testClient(t, srv.URL).List(context.Background(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("list = %d rows, want 1", len(rows))
	}
	if rows[0].Subject != "Hi" || rows[0].From != "boss@example.com" {
		t.Errorf("headers = %+v, want Hi/boss", rows[0])
	}
	if rows[0].Body != "" {
		t.Errorf("list fetched a body: %q", rows[0].Body)
	}
}

func TestSearchNeedsQuery(t *testing.T) {
	srv := stubGmail(t)
	defer srv.Close()
	if _, err := testClient(t, srv.URL).Search(context.Background(), "  ", 0); err == nil {
		t.Fatal("empty query should fail")
	}
	rows, err := testClient(t, srv.URL).Search(context.Background(), "from:boss", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("search = %d rows, want 1", len(rows))
	}
}

func TestReadReturnsBody(t *testing.T) {
	srv := stubGmail(t)
	defer srv.Close()
	m, err := testClient(t, srv.URL).Read(context.Background(), "m1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "hello world" {
		t.Errorf("body = %q, want hello world", m.Body)
	}
	if m.Truncated {
		t.Error("short body must not report truncated")
	}
	if _, err := testClient(t, srv.URL).Read(context.Background(), ""); err == nil {
		t.Fatal("empty id should fail")
	}
}

func TestReadTruncatesLongBody(t *testing.T) {
	srv := stubGmail(t)
	defer srv.Close()
	m, err := testClient(t, srv.URL).Read(context.Background(), "mlong")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Truncated {
		t.Fatal("over-cap body must report truncated")
	}
	if len(m.Body) != MaxBody+len("…") {
		t.Errorf("body len = %d, want %d+marker", len(m.Body), MaxBody)
	}
}

func TestCreateDraftValidates(t *testing.T) {
	srv := stubGmail(t)
	defer srv.Close()
	c := testClient(t, srv.URL)
	for _, tc := range []struct{ to, subj, body string }{
		{"", "s", "b"}, {"a@x.com", "", "b"}, {"a@x.com", "s", ""},
	} {
		if _, err := c.CreateDraft(context.Background(), tc.to, tc.subj, tc.body); err == nil {
			t.Errorf("draft %+v should fail", tc)
		}
	}
	d, err := c.CreateDraft(context.Background(), "a@x.com", "Hi", "body")
	if err != nil {
		t.Fatal(err)
	}
	if d.ID != "d1" {
		t.Errorf("draft id = %q, want d1", d.ID)
	}
}
