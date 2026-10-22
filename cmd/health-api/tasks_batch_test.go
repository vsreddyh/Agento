package main

import (
	"agento/internal/tasks"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBatchRequiresAuth(t *testing.T) {
	t.Setenv("PASSWORD", "test-secret-12345678")
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/batch", strings.NewReader(`{"ops":[]}`))
	w := httptest.NewRecorder()
	batchTasks(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d", w.Code)
	}
}

func TestBatchRejectsGet(t *testing.T) {
	t.Setenv("PASSWORD", "test-secret-12345678")
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/batch", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w := httptest.NewRecorder()
	batchTasks(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get: got %d", w.Code)
	}
}

func TestParseBatchOpsValid(t *testing.T) {
	fields := map[string]any{
		"ops": []any{
			map[string]any{"op": "complete", "id": "abc"},
			map[string]any{"op": "skip", "id": "def", "reason": "busy"},
			map[string]any{"op": "update", "id": "ghi", "fields": map[string]any{"due_date": "2026-12-01"}},
		},
	}
	ops, err := parseBatchOps(fields)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(ops) != 3 || ops[0].Op != "complete" || ops[1].Reason != "busy" {
		t.Fatalf("unexpected ops: %+v", ops)
	}
	if ops[2].Fields["due_date"] != "2026-12-01" {
		t.Fatalf("update fields lost: %+v", ops[2])
	}
}

func TestParseBatchOpsRejectsBadShapes(t *testing.T) {
	cases := map[string]map[string]any{
		"missing ops":       {},
		"null ops":          {"ops": nil},
		"ops not array":     {"ops": "complete"},
		"item not object":   {"ops": []any{"complete"}},
		"missing op":        {"ops": []any{map[string]any{"id": "abc"}}},
		"missing id":        {"ops": []any{map[string]any{"op": "complete"}}},
		"op not string":     {"ops": []any{map[string]any{"op": 3, "id": "abc"}}},
		"id not string":     {"ops": []any{map[string]any{"op": "complete", "id": 3}}},
		"reason not string": {"ops": []any{map[string]any{"op": "skip", "id": "abc", "reason": 3}}},
		"fields not object": {"ops": []any{map[string]any{"op": "update", "id": "abc", "fields": "soon"}}},
	}
	for name, fields := range cases {
		if _, err := parseBatchOps(fields); err == nil {
			t.Fatalf("%s: must fail", name)
		}
	}
}

func TestBatchHandlerRejectsBadShapes(t *testing.T) {
	t.Setenv("PASSWORD", "test-secret-12345678")
	for _, body := range []string{`{}`, `{"ops": "complete"}`, `{"ops": [{"id": "abc"}]}`, `{"ops": [{"op": "complete", "id": "   "}]}`, `{"ops": [{"op": "", "id": "abc"}]}`, `not json`} {
		req := httptest.NewRequest(http.MethodPost, "/api/tasks/batch", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-secret-12345678")
		w := httptest.NewRecorder()
		batchTasks(w, req)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("body %s: got %d, want 422", body, w.Code)
		}
	}
}

func TestParseBatchOpsTrimsAndRequires(t *testing.T) {
	ops, err := parseBatchOps(map[string]any{"ops": []any{
		map[string]any{"op": "skip", "id": "  abc  ", "reason": "  busy  "},
	}})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ops[0].ID != "abc" || ops[0].Reason != "busy" {
		t.Fatalf("op/id/reason must trim, got %+v", ops[0])
	}
	for name, tc := range map[string]struct {
		fields map[string]any
		want   string
	}{
		"missing op":                 {map[string]any{"ops": []any{map[string]any{"id": "abc"}}}, "ops[0].op is required"},
		"empty op":                   {map[string]any{"ops": []any{map[string]any{"op": "  ", "id": "abc"}}}, "ops[0].op is required"},
		"op not text":                {map[string]any{"ops": []any{map[string]any{"op": 3, "id": "abc"}}}, "ops[0].op must be a string"},
		"missing id":                 {map[string]any{"ops": []any{map[string]any{"op": "complete"}}}, "ops[0].id is required"},
		"blank id":                   {map[string]any{"ops": []any{map[string]any{"op": "complete", "id": "   "}}}, "ops[0].id is required"},
		"id not text":                {map[string]any{"ops": []any{map[string]any{"op": "complete", "id": 3}}}, "ops[0].id must be a string"},
		"unknown key":                {map[string]any{"ops": []any{map[string]any{"op": "skip", "id": "abc", "reasons": "busy"}}}, "ops[0].reasons is not a batch field; want op, id, reason, or fields"},
		"wrong level":                {map[string]any{"ops": []any{map[string]any{"op": "update", "id": "abc", "due_date": "2026-12-01"}}}, "ops[0].due_date is not a batch field; want op, id, reason, or fields"},
		"unknown keys deterministic": {map[string]any{"ops": []any{map[string]any{"op": "skip", "id": "abc", "zzz": 1, "aaa": 2}}}, "ops[0].aaa is not a batch field; want op, id, reason, or fields"},
	} {
		_, err := parseBatchOps(tc.fields)
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%s: want %q, got %v", name, tc.want, err)
		}
	}
	for _, fields := range []map[string]any{
		{"ops": []any{map[string]any{"op": "  complete  ", "id": "abc"}}},
	} {
		ops, err := parseBatchOps(fields)
		if err != nil {
			t.Fatalf("surrounding spaces trim, got %v", err)
		}
		if ops[0].Op != "complete" {
			t.Fatalf("want trimmed op, got %+v", ops[0])
		}
	}
}

func TestWriteBatchErrNamesTheFailingOp(t *testing.T) {
	// Conflict keeps its 409 through the batch wrapper, with index/id/op added.
	w := httptest.NewRecorder()
	writeBatchErr(w, &tasks.BatchOpError{Index: 2, Op: "update", ID: "abc",
		Err: &tasks.StoreError{Msg: "conflict: task changed", Conflict: true}})
	if w.Code != http.StatusConflict {
		t.Fatalf("conflict: got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{`"index":2`, `"id":"abc"`, `"op":"update"`, "conflict"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %s: %s", want, body)
		}
	}
	// Unknown id keeps its 404. The flag is what decides it, not the
	// message text.
	w2 := httptest.NewRecorder()
	writeBatchErr(w2, &tasks.BatchOpError{Index: 0, Op: "complete", ID: "missing",
		Err: &tasks.StoreError{Msg: "unknown task 'missing'", NotFound: true}})
	if w2.Code != http.StatusNotFound {
		t.Fatalf("unknown task: got %d", w2.Code)
	}
	if !strings.Contains(w2.Body.String(), `"index":0`) {
		t.Fatalf("body missing index: %s", w2.Body.String())
	}
	// Non-batch errors delegate to the single-verb mapping.
	w3 := httptest.NewRecorder()
	writeBatchErr(w3, errors.New("boom"))
	if w3.Code != http.StatusInternalServerError {
		t.Fatalf("plain error: got %d", w3.Code)
	}
	// No-transaction deployments are a capability problem: 503, not 422.
	w4 := httptest.NewRecorder()
	writeBatchErr(w4, tasks.ErrNoTransactions)
	if w4.Code != http.StatusServiceUnavailable {
		t.Fatalf("no transactions: got %d", w4.Code)
	}
}
