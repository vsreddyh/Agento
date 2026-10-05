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
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
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
		"source":  truncateField(source, 40),
		"detail":  truncateField(detail, 300),
		"at":      primitive.NewDateTimeFromTime(time.Now().UTC()),
	}
	if _, err := s.db.Collection(auditCollection).InsertOne(ctx, entry); err != nil {
		log.Printf("tasks: mutation log write failed (op=%s task=%s): %v", op, taskID, err)
	}
}

// EnsureAuditIndex creates the audit index. Called from EnsureSchema's siblings so
// a deployment that has never logged a mutation still gets the index.
func (s *Store) EnsureAuditIndex(ctx context.Context) error {
	_, err := s.db.Collection(auditCollection).Indexes().CreateMany(ctx,
		[]mongoDrv.IndexModel{
			// Retention prunes by age, exactly like money_transactions.
			{Keys: bson.D{{Key: "at", Value: 1}}, Options: options.Index().SetName("at_1")},
			// "what happened to this task" is the question the log exists for.
			{Keys: bson.D{{Key: "task_id", Value: 1}}, Options: options.Index().SetName("task_id_1")},
		})
	return err
}

func truncateField(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
