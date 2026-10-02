package tasks

import (
	"context"
	"time"

	"agento/internal/mongostore"
	"go.mongodb.org/mongo-driver/bson"
)

// Rollover policy: what completing a task does about its own recurrence.
// A structured cadence mints its next occurrence here; a custom condition
// is left to the caller (see skills/task-manager/SKILL.md). Split out of
// store.go (#171); still a Store method, because minting writes.

func (s *Store) rollOver(ctx context.Context, done map[string]any) (map[string]any, error) {
	rep := repeatFromDoc(bson.M(done))
	if !rep.IsStructured() {
		return nil, nil
	}
	if err := rep.Validate(); err != nil {
		return nil, err
	}
	dueDate, _ := done["due_date"].(string)
	dueTime, _ := done["due_time"].(string)
	nextDate, ok := rep.NextDueDate(dueDate, time.Now().UTC())
	if !ok {
		return nil, nil
	}
	name, _ := done["name"].(string)
	description, _ := done["description"].(string)
	// Typed reads, not defaults: a legacy row with an unparseable field must
	// fail the rollover (logged) rather than roll over with reset values.
	mins, ok := mongostore.ToInt(done["estimated_minutes"])
	if !ok {
		return nil, fail("cannot roll over: estimated_minutes is not a number")
	}
	parallel, ok := mongostore.ToBool(done["parallelable"])
	if !ok {
		return nil, fail("cannot roll over: parallelable is not a boolean")
	}
	return s.Create(ctx, name, description, nextDate, dueTime, &mins, rep, &parallel)
}
