package tasks

// Canonical task duplication: a user copy, not a rollover successor.
//
// Duplicate reads the source row and re-enters it through CreateWithKey, so
// every validation the create path enforces (required fields, due shapes,
// recurrence rules) applies to the copy too — a corrupt source fails loudly
// instead of cloning garbage. The copy is a new open task: fresh id and
// revision, no completion, no skip marker, and no `rolled_from` back-link
// (that marker answers "did this repeat roll over", which a hand-made copy
// is not). What IS carried: name, description, voice phrase, due date/time,
// estimated minutes, the whole recurrence, and parallelable.

import (
	"context"
	"strings"

	"agento/internal/mongostore"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
)

// Duplicate copies a task — open, done, or skipped — as a new open task.
func (s *Store) Duplicate(ctx context.Context, id string) (map[string]any, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("bad id '%s'", id)
	}
	var cur bson.M
	if err := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&cur); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, fail("unknown task '%s'", id)
		}
		return nil, err
	}
	// Typed reads, asserted like the rollover plan's: a hand-edited row with
	// the wrong types must fail here, not copy as zeros.
	mins, ok := mongostore.ToInt(cur["estimated_minutes"])
	if !ok {
		return nil, fail("cannot duplicate: estimated_minutes is not a number")
	}
	parallel, ok := mongostore.ToBool(cur["parallelable"])
	if !ok {
		return nil, fail("cannot duplicate: parallelable is not a boolean")
	}
	name, _ := cur["name"].(string)
	description, _ := cur["description"].(string)
	speakName, _ := cur["speak_name"].(string)
	dueDate, _ := cur["due_date"].(string)
	dueTime, _ := cur["due_time"].(string)
	doc, _, err := s.CreateWithKey(ctx, name, description, dueDate, dueTime,
		&mins, repeatFromDoc(cur), &parallel, speakName, "", "", "")
	if err != nil {
		return nil, err
	}
	return doc, nil
}
