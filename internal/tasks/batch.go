package tasks

// Atomic task batches: one bulk run, one transaction (#344).
//
// A bulk run (complete, skip, move, or duplicate a set of selected tasks)
// maps to one Batch call, which commits fully or not at all. Every op runs through the
// same CompleteDetail / SkipDetail / Update the single-task paths use, so the
// batch cannot drift from what one task does alone — the only differences
// are deliberate and named below:
//
//   - one change-marker bump per committed batch, not one per op. The per-op
//     bumps are suppressed for the batch (see noBumpCtx) and a single bump
//     lands inside the transaction, so the marker advances exactly when the
//     batch commits and never on abort.
//   - a failed rollover ABORTS the batch. Single verbs report a failed
//     rollover as success-with-RolloverFailed (the parent is already done, so
//     failing the call would lie); inside a batch nothing has committed yet,
//     so aborting is both honest and atomic.
//   - no transaction support means no batch. The sequential fallback other
//     stores use would silently break the all-or-nothing contract, so Batch
//     refuses instead of degrading.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	mongoDrv "go.mongodb.org/mongo-driver/mongo"
)

// BatchMaxOps caps one batch. One bulk run is capped client-side at the same
// number, so a whole selection always fits in a single transaction and no
// chunking protocol is needed on either side.
const BatchMaxOps = 50

// BatchOp is one operation inside a batch: complete or skip a task by id, or
// update it with the same field map the single-task update takes (due_date
// for a move, expected_revision for the concurrency guard, skip reason only
// on skip).
type BatchOp struct {
	Op     string
	ID     string
	Reason string
	Fields map[string]any
}

// BatchItemResult is what one committed op produced: the fresh task, the
// minted successor when a structured cadence rolled over, and the rollover
// outcome with its field-specific detail (empty unless the rollover stopped).
type BatchItemResult struct {
	Index    int
	ID       string
	Op       string
	Task     map[string]any
	Next     map[string]any
	Rollover Rollover
	Detail   string
}

// BatchOpError names which op of a batch failed, keeping the original error
// (and its 404/409 mapping) intact underneath for errors.As.
type BatchOpError struct {
	Index int
	Op    string
	ID    string
	Err   error
}

func (e *BatchOpError) Error() string {
	return fmt.Sprintf("batch op %d (%s %s): %v", e.Index, e.Op, e.ID, e.Err)
}

func (e *BatchOpError) Unwrap() error { return e.Err }

// noBumpKey suppresses per-op change-marker bumps inside a batch. The flag
// rides the context because the bump sites are shared: insert is the funnel
// for creates AND rollover successors, and CompleteDetail / SkipDetail /
// Update each bump for their own op. Threading a bool through all of those
// signatures would churn every caller to serve the one caller that batches;
// the context carries it to exactly the writes one batch performs and
// nowhere else.
type noBumpKey struct{}

func noBump(ctx context.Context) context.Context {
	return context.WithValue(ctx, noBumpKey{}, true)
}

// useTransactions probes for replica-set support, mirroring the money
// store's transact pattern: Atlas answers with a set name, a standalone
// does not, and only the answer — never an error — decides.
func (s *Store) useTransactions(ctx context.Context) bool {
	var hello map[string]any
	if err := s.client.Database("admin").RunCommand(ctx,
		map[string]any{"hello": 1}).Decode(&hello); err != nil {
		return false
	}
	if name, _ := hello["setName"].(string); name != "" {
		return true
	}
	if msg, _ := hello["msg"].(string); msg == "isdbgrid" {
		return true
	}
	return false
}

// transact runs fn in a transaction with retry on transient errors. Unlike
// the money store there is no non-transactional fallback: a batch that
// cannot be atomic is refused by Batch before this is ever reached.
func (s *Store) transact(ctx context.Context, fn func(ctx context.Context) error) error {
	for attempt := 1; ; attempt++ {
		sess, err := s.client.StartSession()
		if err != nil {
			return err
		}
		err = func() error {
			defer sess.EndSession(ctx)
			_, txErr := sess.WithTransaction(ctx, func(sc mongoDrv.SessionContext) (any, error) {
				return nil, fn(sc)
			})
			return txErr
		}()
		if err == nil {
			return nil
		}
		var cmdErr mongoDrv.CommandError
		if errors.As(err, &cmdErr) {
			transient := false
			for _, l := range cmdErr.Labels {
				if l == "TransientTransactionError" {
					transient = true
				}
				if l == "UnknownTransactionCommitResult" {
					return err // commit ambiguous — never re-run the body
				}
			}
			if transient && attempt < 5 {
				continue
			}
		}
		return err
	}
}

// Batch applies ops atomically: every op commits or none does. A hard error
// on any op (unknown id, revision conflict, validation failure, failed
// rollover) aborts the whole batch and names the failing op; an empty batch,
// an unknown op, and an over-cap batch are rejected before any write.
func (s *Store) Batch(ctx context.Context, ops []BatchOp) ([]BatchItemResult, error) {
	if len(ops) == 0 {
		return nil, fail("batch is empty: send 1-%d ops", BatchMaxOps)
	}
	if len(ops) > BatchMaxOps {
		return nil, fail("batch holds %d ops, over the %d-op cap; select fewer tasks", len(ops), BatchMaxOps)
	}
	for i, op := range ops {
		switch op.Op {
		case "complete", "skip", "update", "duplicate":
		default:
			return nil, &BatchOpError{Index: i, Op: op.Op, ID: op.ID,
				Err: fail("unknown batch op %q: want complete, skip, update, or duplicate", op.Op)}
		}
		if strings.TrimSpace(op.ID) == "" {
			return nil, &BatchOpError{Index: i, Op: op.Op, ID: op.ID,
				Err: fail("batch op %d (%s) has no id", i, op.Op)}
		}
	}
	if !s.useTransactions(ctx) {
		return nil, fail("batch unavailable: this deployment does not support transactions")
	}
	results := make([]BatchItemResult, 0, len(ops))
	err := s.transact(ctx, func(sc context.Context) error {
		// Per-op bumps suppressed: every shared write path (the verbs, the
		// insert funnel, rollover successors) checks this flag, and the one
		// bump for the whole batch lands below, inside the transaction.
		nb := noBump(sc)
		for i, op := range ops {
			res := BatchItemResult{Index: i, ID: op.ID, Op: op.Op}
			var err error
			switch op.Op {
			case "complete":
				var next map[string]any
				var reason Rollover
				var detail string
				res.Task, next, reason, detail, err = s.CompleteDetail(nb, op.ID)
				res.Next, res.Rollover, res.Detail = next, reason, detail
			case "skip":
				var next map[string]any
				var reason Rollover
				var detail string
				res.Task, next, reason, detail, err = s.SkipDetail(nb, op.ID, op.Reason)
				res.Next, res.Rollover, res.Detail = next, reason, detail
			case "update":
				res.Task, err = s.Update(nb, op.ID, op.Fields)
				res.Rollover = RolloverNone
			case "duplicate":
				res.Task, err = s.Duplicate(nb, op.ID)
				res.Rollover = RolloverNone
			}
			if err != nil {
				return &BatchOpError{Index: i, Op: op.Op, ID: op.ID, Err: err}
			}
			// A failed rollover aborts: unlike a single verb, nothing has
			// committed yet, so the parent must not be left done without
			// its successor. The field detail names what to fix.
			if res.Rollover == RolloverFailed {
				return &BatchOpError{Index: i, Op: op.Op, ID: op.ID,
					Err: fail("rollover failed: %s", res.Detail)}
			}
			results = append(results, res)
		}
		s.bumpChanges(sc)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}
