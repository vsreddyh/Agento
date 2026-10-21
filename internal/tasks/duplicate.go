package tasks

// Canonical task duplication: a user duplicate, not a rollover successor.
//
// Duplicate reads the source row and re-enters it through CreateWithKey, so
// every validation the create path enforces (required fields, due shapes,
// recurrence rules) applies to the duplicate too — a corrupt source fails
// loudly instead of duplicating garbage. The duplicate is a new open task:
// fresh id and revision, no completion, no skip marker, and no `rolled_from`
// back-link (that marker answers "did this repeat roll over", which a
// hand-made duplicate is not). What IS carried: name, description, voice
// phrase, due date/time, estimated minutes, the whole recurrence, and
// parallelable.

import (
	"context"
	"strings"

	"agento/internal/mongostore"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
)

// Duplicate makes a new open task from any existing one — open, done, or
// skipped.
func (s *Store) Duplicate(ctx context.Context, id string) (map[string]any, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, badID(id)
	}
	var cur bson.M
	if err := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&cur); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, unknownTask(id)
		}
		return nil, err
	}
	// Typed reads, asserted like the rollover plan's: a hand-edited row with
	// the wrong types must fail here, not duplicate as zeros. ABSENT is the
	// exception, not the rule: it reads as the zero value, the same leniency
	// the store gives a missing `revision` (the backfill) — a legacy row the
	// user can still complete must also duplicate. Present-but-null counts
	// as absent, like everywhere else null does.
	strField := func(key string) (string, error) {
		v, present := cur[key]
		if !present || v == nil {
			return "", nil
		}
		s, ok := v.(string)
		if !ok {
			return "", fail("cannot duplicate: %s is not a string", key)
		}
		return s, nil
	}
	name, err := strField("name")
	if err != nil {
		return nil, err
	}
	description, err := strField("description")
	if err != nil {
		return nil, err
	}
	speakName, err := strField("speak_name")
	if err != nil {
		return nil, err
	}
	dueDate, err := strField("due_date")
	if err != nil {
		return nil, err
	}
	dueTime, err := strField("due_time")
	if err != nil {
		return nil, err
	}
	mins := 0
	if v, present := cur["estimated_minutes"]; present && v != nil {
		var ok bool
		mins, ok = mongostore.ToInt(v)
		if !ok {
			return nil, fail("cannot duplicate: estimated_minutes is not a number")
		}
	}
	parallel := false
	if v, present := cur["parallelable"]; present && v != nil {
		var ok bool
		parallel, ok = mongostore.ToBool(v)
		if !ok {
			return nil, fail("cannot duplicate: parallelable is not a boolean")
		}
	}
	doc, _, err := s.CreateWithKey(ctx, name, description, dueDate, dueTime,
		&mins, repeatFromDoc(cur), &parallel, speakName, "", "", "")
	// No idempotency key, source, or fingerprint: a duplicate is a NEW
	// write by definition, so it must never dedupe against — or replay as
	// — any previous task.
	if err != nil {
		return nil, err
	}
	return doc, nil
}
