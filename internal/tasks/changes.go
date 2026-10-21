package tasks

// Change marker (#178).
//
// The problem this exists for: the agent and the app write the same
// collection, and nothing tells the app that a write happened. The app's own
// mutations re-render because they happen in-process, so every surface looked
// up to date — while a task the agent created or completed over MCP was
// invisible until something else forced a reload. The two writers are
// indistinguishable as far as anyone can see.
//
// So every write this store commits advances ONE counter, in its own
// collection, and the app can ask what the counter is. The store owns the
// counter on purpose: `insert` is the funnel that Create, CreateWithKey and
// rollOver's successor all pass through, and the transports (HTTP, MCP) each
// had their own list of "what counts as a change" — which is exactly how the
// rollover successor escaped the audit log for two releases (#180). One
// place, one rule, no transport to forget it.
//
// The marker is deliberately NOT the audit log. `task_mutations` is an
// append-only history with 90-day retention, best-effort, and it is called BY
// the transports; this is a single upserted document the store maintains
// itself. A log answers "what happened to this task"; a counter answers
// "is there anything new", which is the question with no other cheap answer.
//
// Best-effort, exactly like the audit write: a marker bump that fails must not
// fail a task write that already committed. The consequence of a lost bump is
// that the next poll sees a stale revision and refreshes when it need not —
// the failure direction is a redundant fetch, not a stale screen, because
// every refresh already re-reads the whole list.

import (
	"context"
	"errors"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// changesCollection is separate from `tasks` for the same reason the audit log
// is: it must outlive any single task, and a document that only ever counts
// does not belong inside the collection it is counting.
const changesCollection = "tasks_changes"

// changesDocID is the single document in changesCollection. One document, not
// one per task: the question being answered is collection-wide ("did anything
// change?"), and an upserted counter needs no index beyond the _id it already
// has — which is why EnsureSchema has nothing to add for it.
const changesDocID = "revision"

// changeWriteBudget bounds one bump, independently of the request that
// triggered it. Same shape, and same reason, as auditWriteBudget: the caller's
// context is cancelled the moment its client disconnects, and the interesting
// case is a mutation that DID commit moments before.
const changeWriteBudget = 5 * time.Second

// bumpChanges advances the collection-wide change marker. Best-effort — see
// the package comment. A single upserted $inc, so a lost bump is the failure
// mode that degrades to a redundant refresh rather than to a stale one.
//
// Inside a batch the flag from batch.go suppresses this: the batch bumps
// once for all of its writes instead of once per op, so per-op bumps
// (including rollover successors through the insert funnel) stay silent
// until the batch's own bump lands.
func (s *Store) bumpChanges(ctx context.Context) {
	if ctx.Value(noBumpKey{}) != nil {
		return
	}
	bumpCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), changeWriteBudget)
	defer cancel()
	// $inc with an upsert is the whole implementation: the first bump creates
	// the document at revision 1, every later one is an atomic increment on
	// that same _id. MongoDB serialises updates on one document, so
	// concurrent writers cannot lose an increment.
	if _, err := s.db.Collection(changesCollection).UpdateOne(bumpCtx,
		bson.M{"_id": changesDocID},
		bson.M{
			"$inc": bson.M{"revision": 1},
			"$set": bson.M{"at": primitive.NewDateTimeFromTime(time.Now().UTC())},
		},
		options.Update().SetUpsert(true),
	); err != nil {
		// Loudly, not silently: an invisible gap in the marker would read as
		// "the agent's write is not showing up", which is the bug this file
		// exists to make impossible.
		log.Printf("tasks: change marker bump failed: %v", err)
	}
}

// ChangeRevision returns the collection-wide change marker: the counter every
// task write advances, and the time of the last advance.
//
// A collection that has never been written reports revision 0 and a zero
// time, with no error — there is no marker yet, but there is also nothing
// that could have changed, and both are the same answer to a caller.
func (s *Store) ChangeRevision(ctx context.Context) (revision int64, at time.Time, err error) {
	var doc struct {
		Revision int64              `bson:"revision"`
		At       primitive.DateTime `bson:"at"`
	}
	err = s.db.Collection(changesCollection).
		FindOne(ctx, bson.M{"_id": changesDocID}).Decode(&doc)
	if err != nil {
		if errors.Is(err, mongoDrv.ErrNoDocuments) {
			return 0, time.Time{}, nil
		}
		return 0, time.Time{}, err
	}
	return doc.Revision, doc.At.Time().UTC(), nil
}
