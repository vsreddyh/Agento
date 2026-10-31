package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agento/internal/connectors"

	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
)

// stubGmail records list query params and serves list/get/draft shapes.
type stubGmailCalls struct {
	query    string
	labelIds string
	max      string
	format   string
}

// Stub Gmail wire: list/get/draft shapes only, no auth.
func stubGmail(t *testing.T, calls *stubGmailCalls) *httptest.Server {
	t.Helper()
	body := base64.URLEncoding.EncodeToString([]byte("hello world"))
	rawBody := base64.RawURLEncoding.EncodeToString([]byte("unpadded hello"))
	long := strings.Repeat("x", MaxBody+10)
	longEnc := base64.URLEncoding.EncodeToString([]byte(long))
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages"):
			if calls != nil {
				calls.query, calls.labelIds, calls.max = q.Get("q"), q.Get("labelIds"), q.Get("maxResults")
			}
			fmt.Fprint(w, `{"messages":[{"id":"m1","threadId":"t1"},{"id":"mgone","threadId":"t1"}],"resultSizeEstimate":2}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages/m1"):
			if calls != nil {
				calls.format = q.Get("format")
			}
			fmt.Fprintf(w, `{"id":"m1","threadId":"t1","snippet":"hello","payload":{`+
				`"mimeType":"multipart/alternative","headers":[`+
				`{"name":"Subject","value":"Hi"},{"name":"From","value":"boss@example.com"},{"name":"Date","value":"Mon, 26 Oct 2026 09:00:00 +0000"}],`+
				`"parts":[{"mimeType":"text/plain","body":{"data":%q}}]}}`, body)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages/mraw"):
			fmt.Fprintf(w, `{"id":"mraw","threadId":"t1","snippet":"u","payload":{`+
				`"mimeType":"text/plain","headers":[],"body":{"data":%q}}}`, rawBody)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages/mgone"):
			http.Error(w, "gone", http.StatusNotFound)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages/mlong"):
			fmt.Fprintf(w, `{"id":"mlong","threadId":"t1","snippet":"x","payload":{`+
				`"mimeType":"text/plain","headers":[],"body":{"data":%q}}}`, longEnc)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages/mnull"):
			fmt.Fprint(w, `null`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages/mmixed"):
			fmt.Fprintf(w, `{"id":"mmixed","threadId":"t1","snippet":"mix","payload":{`+
				`"mimeType":"multipart/mixed","headers":[null,{"name":"Subject","value":"Mixed"}],`+
				`"parts":[null,{"mimeType":"text/plain","body":{"data":%q}}]}}`, body)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages/mhtml"):
			htmlBody := base64.RawURLEncoding.EncodeToString([]byte("<b>hi</b>"))
			fmt.Fprintf(w, `{"id":"mhtml","threadId":"t1","snippet":"<b>hi</b>","payload":{`+
				`"mimeType":"text/html","headers":[],"body":{"data":%q}}}`, htmlBody)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages/menc"):
			encBody := base64.RawURLEncoding.EncodeToString([]byte("hi"))
			fmt.Fprintf(w, `{"id":"menc","threadId":"t1","snippet":"e","payload":{`+
				`"mimeType":"text/plain","headers":[{"name":"Subject","value":"=?utf-8?q?caf=C3=A9?="}],`+
				`"body":{"data":%q}}}`, encBody)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages/mnested"):
			nestedBody := base64.RawURLEncoding.EncodeToString([]byte("nested hello"))
			fmt.Fprintf(w, `{"id":"mnested","threadId":"t1","snippet":"nested","payload":{`+
				`"mimeType":"multipart/mixed","headers":[{"name":"Subject","value":"Nested"}],`+
				`"parts":[{"mimeType":"multipart/alternative","parts":[`+
				`{"mimeType":"text/plain","body":{"data":%q}},`+
				`{"mimeType":"text/html","body":{"data":%q}}]}]}}`, nestedBody, nestedBody)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages/morder"):
			htmlFirst := base64.RawURLEncoding.EncodeToString([]byte("<b>first</b>"))
			plainSecond := base64.RawURLEncoding.EncodeToString([]byte("second wins"))
			fmt.Fprintf(w, `{"id":"morder","threadId":"t1","snippet":"o","payload":{`+
				`"mimeType":"multipart/mixed","headers":[],"parts":[`+
				`{"mimeType":"multipart/alternative","parts":[{"mimeType":"text/html","body":{"data":%q}}]},`+
				`{"mimeType":"text/plain","body":{"data":%q}}]}}`, htmlFirst, plainSecond)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages/mforward"):
			fwdBody := base64.RawURLEncoding.EncodeToString([]byte("forwarded hello"))
			fmt.Fprintf(w, `{"id":"mforward","threadId":"t1","snippet":"f","payload":{`+
				`"mimeType":"multipart/mixed","headers":[{"name":"Subject","value":"Fwd"}],`+
				`"parts":[{"mimeType":"message/rfc822","parts":[{`+
				`"mimeType":"multipart/alternative","parts":[{`+
				`"mimeType":"text/plain","body":{"data":%q}}]}]}]}}`, fwdBody)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/users/me/messages/mbig"):
			bigBody := base64.RawURLEncoding.EncodeToString([]byte("readable prefix"))
			fmt.Fprintf(w, `{"id":"mbig","threadId":"t1","snippet":"b","payload":{`+
				`"mimeType":"text/plain","headers":[],`+
				`"body":{"data":%q,"attachmentId":"att123"}}}`, bigBody)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/users/me/drafts"):
			var draft struct {
				Message struct {
					Raw string `json:"raw"`
				} `json:"message"`
			}
			if err := json.NewDecoder(r.Body).Decode(&draft); err != nil {
				t.Errorf("draft body not JSON: %v", err)
				break
			}
			raw, err := base64.RawURLEncoding.DecodeString(draft.Message.Raw)
			if err != nil {
				t.Errorf("draft Raw not base64url: %v", err)
				break
			}
			s := string(raw)
			if !strings.Contains(s, "Subject: ") {
				t.Errorf("draft Raw missing subject: %q", s)
			}
			if strings.Contains(s, "To: minimal@example.com") {
				// Minimal error-adjacent response: no message block.
				// Must fail the call, never panic the server.
				fmt.Fprint(w, `{"id":"d0"}`)
				break
			}
			if !strings.Contains(s, "To: a@x.com") {
				t.Errorf("draft Raw missing recipient: %q", s)
			}
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
	var calls stubGmailCalls
	srv := stubGmail(t, &calls)
	defer srv.Close()
	res, err := testClient(t, srv.URL).List(context.Background(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	// mgone 404s mid-list: skipped with Partial set, m1 still served.
	if len(res.Messages) != 1 {
		t.Fatalf("list = %d rows, want 1 (gone skipped)", len(res.Messages))
	}
	if !res.Partial {
		t.Error("skipped row must report partial")
	}
	if res.Messages[0].Subject != "Hi" || res.Messages[0].From != "boss@example.com" {
		t.Errorf("headers = %+v, want Hi/boss", res.Messages[0])
	}
	if res.Messages[0].Body != "" {
		t.Errorf("list fetched a body: %q", res.Messages[0].Body)
	}
	if calls.format != "metadata" {
		t.Errorf("list rows fetched with format=%q, want metadata", calls.format)
	}
	if calls.labelIds != "INBOX" || calls.max != "10" {
		t.Errorf("list query labelIds=%q max=%q, want INBOX/10", calls.labelIds, calls.max)
	}
}

func TestSearchNeedsQuery(t *testing.T) {
	var calls stubGmailCalls
	srv := stubGmail(t, &calls)
	defer srv.Close()
	if _, err := testClient(t, srv.URL).Search(context.Background(), "  ", 0); err == nil {
		t.Fatal("empty query should fail")
	}
	res, err := testClient(t, srv.URL).Search(context.Background(), "from:boss", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("search = %d rows, want 1", len(res.Messages))
	}
	if calls.query != "from:boss" {
		t.Errorf("query = %q, want pass-through", calls.query)
	}
}

func TestReadReturnsBody(t *testing.T) {
	srv := stubGmail(t, nil)
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

func TestReadNullMessageFailsClosed(t *testing.T) {
	srv := stubGmail(t, nil)
	defer srv.Close()
	if _, err := testClient(t, srv.URL).Read(context.Background(), "mnull"); err == nil {
		t.Fatal("null upstream message should fail, not panic")
	}
}

// Null header/part elements are skipped, never dereferenced.
func TestReadSkipsNullParts(t *testing.T) {
	srv := stubGmail(t, nil)
	defer srv.Close()
	m, err := testClient(t, srv.URL).Read(context.Background(), "mmixed")
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "hello world" || m.Subject != "Mixed" {
		t.Errorf("mixed = %+v, want decoded body + subject", m)
	}
	if m.IsHTML {
		t.Error("plain fallback must not report HTML")
	}
}

// A nested container (mixed → alternative → plain), the standard Gmail
// shape for a message with an attachment, must decode — not skip the
// middle layer for failing the immediate-child MIME filter.
func TestReadDescendsIntoNestedContainers(t *testing.T) {
	srv := stubGmail(t, nil)
	defer srv.Close()
	m, err := testClient(t, srv.URL).Read(context.Background(), "mnested")
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "nested hello" || m.IsHTML {
		t.Errorf("nested = %+v, want plain body, not HTML", m)
	}
}

// Document order must not beat the plain-first contract: an HTML-only
// container before a plain sibling still yields the plain body unflagged.
func TestReadPrefersLaterPlainOverEarlierHTML(t *testing.T) {
	srv := stubGmail(t, nil)
	defer srv.Close()
	m, err := testClient(t, srv.URL).Read(context.Background(), "morder")
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "second wins" || m.IsHTML {
		t.Errorf("ordered = %+v, want later plain body, not earlier HTML", m)
	}
}

// Forwarded mail nests a layer deeper (mixed → rfc822 → alternative):
// any container type must be transparent to the walkers.
func TestReadDescendsIntoForwardedMessage(t *testing.T) {
	srv := stubGmail(t, nil)
	defer srv.Close()
	m, err := testClient(t, srv.URL).Read(context.Background(), "mforward")
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "forwarded hello" || m.IsHTML {
		t.Errorf("forwarded = %+v, want plain body, not HTML", m)
	}
}

// HTML-only mail surfaces markup with the flag set, not silently as prose.
// A text part carrying both readable Data and an attachmentId (large
// inline continuation shape) decodes instead of being skipped.
func TestReadDecodesDataWithAttachmentId(t *testing.T) {
	srv := stubGmail(t, nil)
	defer srv.Close()
	m, err := testClient(t, srv.URL).Read(context.Background(), "mbig")
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "readable prefix" || m.IsHTML {
		t.Errorf("big-inline = %+v, want decoded body, not skipped", m)
	}
}

// RFC2047-encoded subjects decode for the agent, matching the compose
// side that Q-encodes on the way out.
func TestReadDecodesEncodedSubject(t *testing.T) {
	srv := stubGmail(t, nil)
	defer srv.Close()
	m, err := testClient(t, srv.URL).Read(context.Background(), "menc")
	if err != nil {
		t.Fatal(err)
	}
	if m.Subject != "caf\u00e9" {
		t.Errorf("subject = %q, want decoded", m.Subject)
	}
}

func TestReadFlagsHTMLFallback(t *testing.T) {
	srv := stubGmail(t, nil)
	defer srv.Close()
	m, err := testClient(t, srv.URL).Read(context.Background(), "mhtml")
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "<b>hi</b>" || !m.IsHTML {
		t.Errorf("html fallback = %+v, want body + is_html", m)
	}
}

func TestReadDecodesUnpaddedBase64(t *testing.T) {
	srv := stubGmail(t, nil)
	defer srv.Close()
	m, err := testClient(t, srv.URL).Read(context.Background(), "mraw")
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "unpadded hello" {
		t.Errorf("body = %q, want unpadded decode", m.Body)
	}
}

func TestReadTrimsPaddedBase64(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Wrapped lines: whitespace at the ends AND the interior, as
		// Gmail occasionally emits. Trimming ends only would leave the
		// interior newlines undecodable.
		raw := base64.RawURLEncoding.EncodeToString([]byte("padded hello"))
		padded := "\n  " + raw[:10] + "\n" + raw[10:] + "  \n"
		fmt.Fprintf(w, `{"id":"mpad","snippet":"p","payload":{"mimeType":"text/plain","headers":[],"body":{"data":%q}}}`, padded)
	}))
	defer srv.Close()
	m, err := testClient(t, srv.URL).Read(context.Background(), "mpad")
	if err != nil {
		t.Fatal(err)
	}
	if m.Body != "padded hello" {
		t.Errorf("body = %q, want whitespace-tolerant decode", m.Body)
	}
}

// Whitespace-only payloads strip to nothing: missing data, not an
// empty success.
func TestDecodeB64RejectsBlank(t *testing.T) {
	for _, data := range []string{"", "   ", "\n \t\n"} {
		if s, ok := decodeB64(data); ok || s != "" {
			t.Errorf("decodeB64(%q) = %q,%v; want not-ok", data, s, ok)
		}
	}
}

func TestReadTruncatesLongBody(t *testing.T) {
	srv := stubGmail(t, nil)
	defer srv.Close()
	m, err := testClient(t, srv.URL).Read(context.Background(), "mlong")
	if err != nil {
		t.Fatal(err)
	}
	if !m.Truncated {
		t.Fatal("over-cap body must report truncated")
	}
	if n := len([]rune(m.Body)); n != MaxBody+1 {
		t.Errorf("body runes = %d, want %d+marker", n, MaxBody)
	}
}

func TestCreateDraftValidates(t *testing.T) {
	srv := stubGmail(t, nil)
	defer srv.Close()
	c := testClient(t, srv.URL)
	for _, tc := range []struct {
		to, subj, body string
		ok             bool
	}{
		{"", "s", "b", false},
		{"a@x.com", "", "b", false},
		{"a@x.com", "s", "", false},
		{"not-an-email", "s", "b", false},
		{"a@x.com", "bad\nsubject", "b", false},
		{"a@x.com", strings.Repeat("s", MaxSubject+1), "b", false},
		{"a@x.com", "Hi", strings.Repeat("b", MaxBody+1), false},
		{"caf\u00e9@x.com", "s", "b", false},
		{"a@x.com b@y.com", "s", "b", false},
		{"a@x.com,b@y.com", "s", "b", false},
		{"a@x\nBcc: victim@y.com", "s", "b", false},
		{"John Doe <a@x.com>", "s", "b", false},
		{"a@x.com", "Hi", "body", true},
	} {
		_, err := c.CreateDraft(context.Background(), tc.to, tc.subj, tc.body)
		if tc.ok && err != nil {
			t.Errorf("draft %+v should succeed: %v", tc, err)
		}
		if !tc.ok && err == nil {
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
	// Non-ASCII subjects are RFC2047-encoded, not raw UTF-8.
	if _, err := c.CreateDraft(context.Background(), "a@x.com", "caf\u00e9", "body"); err != nil {
		t.Fatalf("non-ascii subject should send: %v", err)
	}
	// Minimal upstream response (no message block) fails the call
	// instead of panicking on a nil dereference.
	if _, err := c.CreateDraft(context.Background(), "minimal@example.com", "Hi", "body"); err == nil {
		t.Fatal("message-less draft response should fail")
	}
}

// A nil list entry skips with Partial set instead of panicking the call.
func TestRowsSkipsNilEntries(t *testing.T) {
	srv := stubGmail(t, nil)
	defer srv.Close()
	c := testClient(t, srv.URL)
	res, err := c.rows(context.Background(), c.static, []*gmail.Message{nil})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Messages) != 0 || !res.Partial {
		t.Errorf("nil entry = %+v, want empty + partial", res)
	}
}

// Construction needs no grant: a disconnected deploy must still boot, and
// the grant resolves per tool call with reconnect guidance.
func TestNewClientNeedsNoGrant(t *testing.T) {
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	key, err := connectors.KeyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	srv := stubGmail(t, nil)
	defer srv.Close()
	c, err := NewClient(connectors.NewMemoryStore(),
		&connectors.OAuthConfig{ClientID: "cid", ClientSecret: "sec", Key: key}, srv.URL, nil)
	if err != nil {
		t.Fatalf("construction without a grant should succeed: %v", err)
	}
	if _, err := c.List(context.Background(), "", 0); err == nil ||
		!strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("grant-less call = %v, want reconnect guidance", err)
	}
}

// An unconfigured deploy still boots: nil cfg constructs, and every
// Google-touching call answers "not configured" per call instead of
// killing the entrypoint probe (#421). Every tool routes through
// service(), so each entry point is pinned here — a future tool that
// bypasses service() fails this test instead of panicking on nil cfg.
func TestNewClientAllowsUnconfigured(t *testing.T) {
	c, err := NewClient(connectors.NewMemoryStore(), nil, "", nil)
	if err != nil {
		t.Fatalf("construction without OAuth config should succeed: %v", err)
	}
	ctx := context.Background()
	unconfigured := map[string]func() error{
		"list": func() error {
			_, err := c.List(ctx, "", 0)
			return err
		},
		"search": func() error {
			_, err := c.Search(ctx, "from:boss", 0)
			return err
		},
		"read": func() error {
			_, err := c.Read(ctx, "msg-id")
			return err
		},
		"create_draft": func() error {
			_, err := c.CreateDraft(ctx, "boss@example.com", "subject", "body")
			return err
		},
	}
	for name, call := range unconfigured {
		if err := call(); err == nil || !strings.Contains(err.Error(), "not configured") {
			t.Fatalf("%s = %v, want not-configured guidance", name, err)
		}
	}
}
