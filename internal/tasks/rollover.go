package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"agento/internal/mongostore"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Rollover policy: what completing a task does about its own recurrence.
// A structured cadence mints its next occurrence here; a custom condition
// is left to the caller (see pi/skills/task-manager/SKILL.md). Split out of
// store.go (#171); still a Store method, because minting writes.

// Rollover is the outcome of a completion's recurrence step, as a named reason
// rather than an absent field (#180).
//
// It used to be `(next == nil)` versus `(next != nil)`, which collapsed four
// distinct outcomes into two answers. "This repeats but no next occurrence was
// created" was indistinguishable from "this task was a one-shot" — so a caller
// could not tell the user the difference, and a genuinely broken repeat looked
// identical to an ordinary completion. That is how four legacy tasks silently
// stopped recurring: the failure was logged to a line nobody read and the API
// reported success.
type Rollover string

const (
	// RolloverCreated: a structured cadence minted the next occurrence.
	RolloverCreated Rollover = "created"
	// RolloverNone: the task is a one-shot. There was never a next to make.
	RolloverNone Rollover = "none"
	// RolloverCustom: the repeat is a condition only the caller can interpret
	// ("every 3rd friday"). The caller owns creating the next occurrence.
	RolloverCustom Rollover = "custom"
	// RolloverExhausted: structured, but no future date is computable — the
	// cadence has run past MaxRollovers, or the stored due_date will not parse.
	RolloverExhausted Rollover = "exhausted"
	// RolloverFailed: minting was attempted and errored. The completion itself
	// succeeded; the recurrence did not. This is the case that needs a human.
	RolloverFailed Rollover = "failed"
)

// UserFacing explains the outcome in words a caller can show without inventing
// its own explanation. Empty for the outcomes that need no explanation.
func (r Rollover) UserFacing() string {
	switch r {
	case RolloverCustom:
		return "this task repeats on a custom condition the server will not interpret; create the next occurrence yourself"
	case RolloverExhausted:
		return "this task repeats, but no future date could be computed from its stored due date or the cadence has run out — check the repeat and re-enter it if it should continue"
	case RolloverFailed:
		return "this task was completed, but creating its next occurrence FAILED — the repeat has stopped until you fix it"
	default:
		return ""
	}
}

// NeedsAttention reports whether a caller should tell the user something. A
// one-shot and a successful rollover are both fine; the other three are not.
func (r Rollover) NeedsAttention() bool {
	return r == RolloverCustom || r == RolloverExhausted || r == RolloverFailed
}

// rolledFromField links a minted occurrence back to the completion that produced
// it. Without it, "did this repeat roll over?" cannot be answered from data —
// which is the whole reason #180's reconciler needs a marker, and why the marker
// is written on the child rather than inferred from name+date (a name and date
// collide with an unrelated task the user created by hand).
const rolledFromField = "rolled_from"

// rollOver mints the next occurrence for a STRUCTURED cadence, or explains why it
// did not. Every non-error path names itself, so no caller has to infer intent
// from an absence.
func (s *Store) rollOver(ctx context.Context, done map[string]any) (map[string]any, Rollover, error) {
	rep := repeatFromDoc(bson.M(done))
	if !rep.IsStructured() {
		if rep.IsZero() {
			return nil, RolloverNone, nil
		}
		return nil, RolloverCustom, nil
	}
	if err := rep.Validate(); err != nil {
		return nil, RolloverFailed, err
	}
	dueDate, _ := done["due_date"].(string)
	dueTime, _ := done["due_time"].(string)
	nextDate, ok := rep.NextDueDate(dueDate, time.Now().UTC())
	if !ok {
		return nil, RolloverExhausted, nil
	}
	name, _ := done["name"].(string)
	description, _ := done["description"].(string)
	// Typed reads, not defaults: a legacy row with an unparseable field must
	// fail the rollover (logged) rather than roll over with reset values.
	mins, ok := mongostore.ToInt(done["estimated_minutes"])
	if !ok {
		return nil, RolloverFailed, fail("cannot roll over: estimated_minutes is not a number")
	}
	parallel, ok := mongostore.ToBool(done["parallelable"])
	if !ok {
		return nil, RolloverFailed, fail("cannot roll over: parallelable is not a boolean")
	}
	next, err := s.Create(ctx, name, description, nextDate, dueTime, &mins, rep, &parallel)
	if err != nil {
		return nil, RolloverFailed, err
	}
	// Back-link the occurrence: mark the SUCCESSOR with the parent's id, so the
	// reconciler can ask "did this completion produce anything?" exactly.
	//
	// The update targets next["id"] — the newly created task. Writing it onto the
	// parent instead is the obvious first mistake and is invisible: the parent
	// really does end up carrying a `rolled_from`, so a quick read of the parent
	// looks correct, while every successor is unlinked and the reconciler reports
	// healthy tasks as broken.
	//
	// Best-effort by design: a failure here means the reconciler cannot prove THIS
	// rollover, but the next task exists and is correct, so it must not fail the
	// completion.
	if hex, ok := done["id"].(string); ok && hex != "" {
		if child, err := primitive.ObjectIDFromHex(fmt.Sprint(next["id"])); err == nil {
			if _, err := s.tasks.UpdateOne(ctx,
				bson.M{"_id": child},
				bson.M{"$set": bson.M{rolledFromField: hex}}); err != nil {
				log.Printf("task %s rolled over but the successor could not be back-linked: %v", hex, err)
			}
		}
	}
	return next, RolloverCreated, nil
}

// RolloverGap is one completed structured repeat with no successor — a task that
// has silently stopped recurring.
type RolloverGap struct {
	TaskID string
	Name   string
	Why    Rollover
}

// ReconcileRollover finds completed STRUCTURED repeats that never produced a
// successor (#180). Nothing else catches these: rollover happens as a side effect
// of Complete, so any writer that does not go through it — a script, a migration,
// a future admin path, or a rollover that failed and was logged — stops the task
// recurring with no error anywhere a user would see.
//
// Read-only. Repair is deliberately NOT attempted here: a task that failed to roll
// over usually failed because its stored data is wrong (no due_time, an
// unparseable date), and inventing a successor would paper over the real problem.
// It reports, and a human fixes it.
func (s *Store) ReconcileRollover(ctx context.Context) ([]RolloverGap, error) {
	// Completed, structured repeat, completed recently enough that a successor
	// should already exist. The window is generous: a task completed a minute ago
	// has not had time to be missed, and an old one is noise by now.
	cutoff := time.Now().UTC().AddDate(0, 0, -1)
	cur, err := s.tasks.Find(ctx, bson.M{
		"completedAt": bson.M{
			"$ne":     nil,
			"$exists": true,
			"$gte":    primitive.NewDateTimeFromTime(cutoff),
			"$lte":    primitive.NewDateTimeFromTime(time.Now().UTC()),
		},
		"repeat_every":  bson.M{"$gt": 0},
		"repeat_unit":   bson.M{"$nin": bson.A{"", nil}},
		"repeat_custom": bson.M{"$ne": true},
	}, options.Find().
		SetSort(bson.D{{Key: "completedAt", Value: -1}}).
		SetLimit(500))
	if err != nil {
		return nil, err
	}
	var done []bson.M
	if err := cur.All(ctx, &done); err != nil {
		return nil, err
	}
	var gaps []RolloverGap
	for _, d := range done {
		id, _ := d["_id"].(primitive.ObjectID)
		// The successor, if there is one, carries the back-link.
		var successor bson.M
		err := s.tasks.FindOne(ctx, bson.M{
			rolledFromField: id.Hex(),
			"completedAt":   nil,
		}).Decode(&successor)
		if err == nil {
			continue // rolled over fine
		}
		if !errors.Is(err, mongoDrv.ErrNoDocuments) {
			return nil, err
		}
		name, _ := d["name"].(string)
		why := RolloverExhausted
		if dueTime, _ := d["due_time"].(string); strings.TrimSpace(dueTime) == "" {
			// The exact shape of the legacy rows that stopped recurring: a
			// structured repeat with no time to carry forward.
			why = RolloverFailed
		}
		gaps = append(gaps, RolloverGap{
			TaskID: id.Hex(),
			Name:   name,
			Why:    why,
		})
	}
	return gaps, nil
}
