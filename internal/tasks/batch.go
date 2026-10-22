package tasks

// Atomic task batches: one bulk run, one transaction (#344).
//
// A bulk run (complete, skip, move, or duplicate a set of selected tasks)
// maps to one Batch call, which commits fully or not at all. Every op runs through the
// same CompleteDetail / SkipDetail / Update / Duplicate the single-task paths use, so the
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
//     refuses with ErrNoTransactions (mapped to 503) instead of degrading.
//   - not idempotent across transport failure. A timeout leaves the outcome
//     unknown (the commit may have landed: see transact), and a blind retry
//     would re-apply — harmless for complete/skip/update, but duplicate
//     would copy twice. On an unknown outcome, refresh via the changes
//     marker and resend the selection instead of retrying blind.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	mongoDrv "go.mongodb.org/mongo-driver/mongo"
)

// BatchMaxOps caps one batch. One bulk run is capped client-side at the same
// number (MAX_BULK_SELECT in TaskManagerScreen.kt — keep the two in sync),
// so a whole selection always fits in a single transaction and no chunking
// protocol is needed on either side.
const BatchMaxOps = 50

// BatchOp is one operation inside a batch: complete, skip, update, or
// duplicate a task by id. Update takes the same field map the single-task
// update takes (due_date for a move, expected_revision for the concurrency
// guard); reason rides only on skip.
type BatchOp struct {
	Op     string
	ID     string
	Reason string
	Fields map[string]any
}

// BatchItemResult is what one committed op produced: the fresh task, the
// minted successor when a structured cadence rolled over, and the rollover
// outcome with its field-specific detail (empty unless the rollover stopped).
//
// ID is the requested id for complete/skip/update — and the NEW copy's id
// for duplicate, whose task is a different row. The response and the audit
// log both name the row the op produced, not the row it was asked about.
type BatchItemResult struct {
	Index    int
	ID       string
	Op       string
	Task     map[string]any
	Next     map[string]any
	Rollover Rollover
	Detail   string
}

// ErrNoTransactions refuses a batch where atomicity is unavailable. A
// distinct sentinel (not a StoreError) so the transport answers 503: this
// is a deployment capability, never a bad envelope.
var ErrNoTransactions = errors.New("batch unavailable: this deployment does not support transactions")

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
//
// Wrapping the session context is safe with this driver: mongo-go-driver
// v1.17.6 recovers the session by context VALUE (sessionFromContext reads
// ctx.Value(sessionKey{}) in mongo/session.go), so WithValue/WithoutCancel
// — which both preserve parent values — keep every write enlisted. The
// abort tests prove it: with the session stripped, the first op would
// autocommit outside the transaction and no abort test could pass.
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
			// Cleanup runs detached: the request ctx may already be
			// cancelled on the slow path, and ending the session must not
			// depend on it.
			defer sess.EndSession(context.Background())
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
// Envelope-level rejections (empty, over-cap) carry Index -1: they name no
// op, and the client toasts the detail without an item number.
func (s *Store) Batch(ctx context.Context, ops []BatchOp) ([]BatchItemResult, error) {
	if len(ops) == 0 {
		return nil, &BatchOpError{Index: -1,
			Err: fail("batch is empty: send 1-%d ops", BatchMaxOps)}
	}
	if len(ops) > BatchMaxOps {
		return nil, &BatchOpError{Index: -1,
			Err: fail("batch holds %d ops, over the %d-op cap; select fewer tasks", len(ops), BatchMaxOps)}
	}
	seen := make(map[string]bool, len(ops))
	for i, op := range ops {
		// Both halves normalized up front: op names are matched
		// case-sensitively downstream, so " Complete " must read the same
		// here as it will in the verb switch — otherwise the transport
		// (which trims) and a direct store caller would disagree on what a
		// padded op means.
		//
		// The ENVELOPE and the ERROR body spell the id two ways on
		// purpose: a success carries the CANONICAL id (lower case, the
		// spelling the change log stores, so a caller can join the two),
		// while an error carries the SENT id (trimmed only, the spelling
		// the caller can go and fix). Both are said out loud here so the
		// difference reads as a decision rather than a drift.
		opName := strings.TrimSpace(op.Op)
		id := strings.TrimSpace(op.ID)
		switch opName {
		case "complete", "skip", "update", "duplicate":
		default:
			// Bounded, like every other caller-controlled echo in this
			// stack: a 10KB op name is not answered with itself, and one
			// mistake reads the same however it was spelled.
			return nil, &BatchOpError{Index: i, Op: opName, ID: id,
				Err: fail("unknown batch op %q: want complete, skip, update, or duplicate",
					TruncateBytes(opName, 40))}
		}
		if id == "" {
			return nil, &BatchOpError{Index: i, Op: opName, ID: id,
				Err: fail("batch op %d (%s) has no id", i, opName)}
		}
		// The seen-key is lowercased: a hex id is case-INSENSITIVE
		// (ObjectIDFromHex accepts upper case, and the log's task_id is
		// lowercased), so "abc…" and "ABC…" name the SAME task. A guard
		// that stops two verbs on one task has to see them as one.
		key := strings.ToLower(id)
		if seen[key] {
			// The same id twice would self-conflict inside one transaction:
			// the second verb reads the first's uncommitted write. Reject
			// up front rather than aborting on our own write. Unreachable
			// from the UI (the selection is a set), so this guards the raw
			// API.
			return nil, &BatchOpError{Index: i, Op: opName, ID: id,
				Err: fail("batch op %d (%s %s) duplicates an earlier op; one op per task", i, opName, id)}
		}
		seen[key] = true
		// Extras ride only where they are read: a `reason` on anything but
		// skip, or `fields` on anything but update, would commit while
		// silently dropping the value — the exact silent-drop shape the
		// single-task layer rejects at its boundary. Empty counts as absent
		// (a blank reason, an empty or all-null fields object), the same
		// "no fields" rule the audit log uses.
		if opName != "skip" && strings.TrimSpace(op.Reason) != "" {
			return nil, &BatchOpError{Index: i, Op: opName, ID: id,
				Err: fail("batch op %d (%s) takes no reason; reason rides only on skip", i, opName)}
		}
		if opName != "update" && ChangedFieldNames(op.Fields) != "no fields" {
			return nil, &BatchOpError{Index: i, Op: opName, ID: id,
				Err: fail("batch op %d (%s) takes no fields; fields ride only on update", i, opName)}
		}
	}
	// Normalized once into a copy: the caller's slice is untouched, and
	// every downstream use (verbs, results, errors) sees the trimmed op and
	// id. The copy is shallow: per-op Fields maps are borrowed read-only
	// (Update and ChangedFieldNames never mutate them), so a future verb
	// that normalizes fields in place must deep-copy first.
	ops = append([]BatchOp(nil), ops...)
	for i := range ops {
		ops[i].ID = strings.TrimSpace(ops[i].ID)
		ops[i].Op = strings.TrimSpace(ops[i].Op)
	}
	if !s.useTransactions(ctx) {
		return nil, ErrNoTransactions
	}
	var results []BatchItemResult
	err := s.transact(ctx, func(sc context.Context) error {
		// Local to the attempt: transact retries this closure on transient
		// errors, and results from a failed attempt must not leak into the
		// next one. Assigned outward only on success, below.
		attempt := make([]BatchItemResult, 0, len(ops))
		wrote := false
		// Per-op bumps suppressed: every shared write path (the verbs, the
		// insert funnel, rollover successors) checks this flag, and the one
		// bump for the whole batch lands below, inside the transaction.
		nb := noBump(sc)
		for i, op := range ops {
			// The id and the op in the envelope are the CANONICAL ones (lower
			// case), the same spelling the change log stores — so a caller
			// diffing the batch answer against a history row does not miss
			// on case, and the transport's switch on res.Op cannot drift
			// from the validator that accepted it. The duplicate branch
			// below replaces the id with the copy's own, which is canonical
			// already.
			res := BatchItemResult{Index: i, ID: strings.ToLower(strings.TrimSpace(op.ID)), Op: strings.ToLower(strings.TrimSpace(op.Op))}
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
				// The audit detail travels with the result rather than
				// being re-derived from op order by the caller.
				fields := op.Fields
				if fields == nil {
					fields = map[string]any{}
				}
				res.Detail = ChangedFieldNames(fields)
			case "duplicate":
				res.Task, err = s.Duplicate(nb, op.ID)
				res.Rollover = RolloverNone
				// The audit detail names the SOURCE the copy came from.
				// The row the log gets is about the copy (its id is the
				// copy's), and without the source the history could say a
				// task appeared without saying what it came from — while
				// the source's own row says nothing about the copy.
				//
				// Carried here rather than re-derived by the transport from
				// the op list, for the same reason the update's detail is
				// carried here: the result should not have to be matched
				// back to its op to know what it was about.
				//
				// Lowercased for convenience, but the SEAM is the
				// authority: RecordMutation canonicalises the detail
				// itself, so this copy is belt and braces, not the rule.
				//
				// Name the row produced, not the row asked about: the
				// copy is a different id, and the response envelope and
				// the audit log both read ID.
				//
				// The source id is set ONLY on success: a failed copy
				// leaves res.ID as the source, and a row that claims a
				// task was duplicated from ITSELF is worse than no row.
				if err == nil {
					if newID, _ := res.Task["id"].(string); newID != "" {
						res.ID = newID
						res.Detail = strings.ToLower(strings.TrimSpace(op.ID))
					}
				}
			default:
				// Unreachable today: the validation loop above accepts
				// exactly the ops this switch names. It is here because
				// the alternative is worse — an op that passes validation
				// but matches no case would fall through with no error,
				// mark the batch as written, and report success for a
				// verb that ran nothing.
				return &BatchOpError{Index: i, Op: op.Op, ID: op.ID,
					Err: fail("batch op %d (%s) is not implemented", i, op.Op)}
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
			// Complete/skip/duplicate always write; an update whose detail
			// is "no fields" took the store's no-op path and wrote nothing.
			if op.Op != "update" || res.Detail != "no fields" {
				wrote = true
			}
			attempt = append(attempt, res)
		}
		// The marker tracks writes, not requests: an all-no-op batch bumps
		// nothing, so the next poll does not refetch over zero change.
		if wrote {
			s.bumpChanges(sc)
		}
		results = attempt
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}
