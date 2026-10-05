package tasks

// Mutation log (#176).
//
// Before this, nothing recorded that a task changed. The agent and the app are
// the same caller as far as the server is concerned — one shared password, one
// collection, no audit — so when a task changed unexpectedly there was no way to
// answer WHEN, from WHERE, or BY WHOM. Every other store in this stack carries a
// `source` field for the same reason; tasks had none.
//
// Append-only and best-effort. A failed write must NOT fail the mutation that
// succeeded: the user asked for a task change, and refusing it because the audit
// record could not be written would be a worse outcome than a gap in the log. So
// every call here swallows its error deliberately, and says so at each site.

import (
	"context"
	"log"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// MutationOps, as recorded. The set is what the routes can do, not what the store
// can do — the log answers "what did a CALLER change", which is the question that
// has no other answer.
const (
	OpCreate   = "create"
	OpUpdate   = "update"
	OpDelete   = "delete"
	OpComplete = "complete"
	OpSkip     = "skip"
	OpReopen   = "reopen"
)

// auditCollection is separate from `tasks` on purpose: it must survive the task.
// A log that goes away with its subject cannot answer "what happened to it".
const auditCollection = "task_mutations"

// RecordMutation appends one entry. Best-effort — see the package comment.
func (s *Store) RecordMutation(ctx context.Context, op, taskID, source, detail string) {
	if op == "" || taskID == "" {
		return
	}
	entry := bson.M{
		"op":      op,
		"task_id": taskID,
		"source":  truncateRunes(source, 40),
		"detail":  truncateRunes(detail, 300),
		"at":      primitive.NewDateTimeFromTime(time.Now().UTC()),
	}
	// The audit write must NOT inherit the caller's cancellation.
	//
	// ctx here is the request context, which Go cancels the moment the client
	// disconnects — and the interesting case is precisely a client that DID: it
	// timed out or hung up, the mutation had already committed, and then the log
	// entry for it was dropped because its context was dead. That inverts the point
	// of the log. "What happened to this task?" is asked most often about the change
	// nobody can explain, and a disconnected client is exactly the sort of change
	// nobody saw happen.
	//
	// WithoutCancel keeps the context's values and drops the cancellation; the short
	// timeout keeps the write bounded so it cannot outlive the process indefinitely.
	// 5s is ample for one indexed insert and, being independent of the request, does
	// not extend the response the client is no longer waiting for.
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteBudget)
	defer cancel()
	if _, err := s.db.Collection(auditCollection).InsertOne(auditCtx, entry); err != nil {
		// Still best-effort: the user's mutation succeeded, so a log gap is the lesser
		// failure. But it is logged loudly rather than swallowed, because a silent gap
		// is indistinguishable from "nothing happened".
		log.Printf("tasks: mutation log write failed (op=%s task=%s): %v", op, taskID, err)
	}
}

// auditWriteBudget bounds one audit insert, independently of the request that
// triggered it. See RecordMutation.
const auditWriteBudget = 5 * time.Second

// ChangedFieldNames names which fields an update touched, for the audit log's `detail`.
//
// It lives here, in the package that owns the log, because BOTH callers had their own
// copy — cmd/health-api had `changedKeys` and cmd/task-manager had
// `changedFieldNames`, identical apart from a comment and the order of two lines. Two
// copies of one rule is the defect shape this stack has produced repeatedly, and it
// matters more here than usual: the two callers are the HTTP API and the agent, so a
// divergence would show up as the app and the agent describing the same edit differently
// in the one log meant to reconcile them.
//
// `expected_revision` is excluded on purpose: it is optimistic-concurrency plumbing, not
// a change to the task. Sorted so the string is stable — Go randomises map iteration, and
// an unstable detail would make two identical updates look different in the log.
func ChangedFieldNames(fields map[string]any) string {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k == "expected_revision" {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return "no fields"
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
