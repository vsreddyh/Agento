package approvals

import (
	"context"
	"errors"
	"time"

	"agento/internal/mongostore"

	"go.mongodb.org/mongo-driver/bson"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoStore is the MongoDB backend for approvals and audit rows.
type MongoStore struct {
	approvals *mongoDrv.Collection
	audit     *mongoDrv.Collection
	// Logf receives operational notes. Nil discards them.
	Logf func(string, ...any)
}

// Shared reports true: Mongo is visible to every process holding the
// same URI, which is what makes relay-held calls decidable from the
// gateway.
func (s *MongoStore) Shared() bool { return true }

func (s *MongoStore) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// New connects to uri/dbName.
func New(uri, dbName string) (*MongoStore, error) {
	_, db, err := mongostore.Open(uri, dbName)
	if err != nil {
		return nil, err
	}
	return &MongoStore{
		approvals: db.Collection(ApprovalsCollection),
		audit:     db.Collection(AuditCollection),
	}, nil
}

// FromEnv connects from the shared MONGODB_URI/MONGODB_DB pair.
func FromEnv() (*MongoStore, error) {
	uri, db := mongostore.Env()
	return New(uri, db)
}

// EnsureIndexes creates the TTL indexes both collections depend on, plus the
// audit lookup. Best-effort at the call site like the runs indexes.
func (s *MongoStore) EnsureIndexes(ctx context.Context) error {
	if _, err := s.approvals.Indexes().CreateMany(ctx, []mongoDrv.IndexModel{
		{
			Keys:    bson.D{{Key: "expiresAt", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(0).SetName("approvals_ttl"),
		},
		{
			// ListApprovals filters status + expiresAt and sorts createdAt:
			// without this it is a COLLSCAN on every app poll.
			Keys: bson.D{
				{Key: "status", Value: 1},
				{Key: "expiresAt", Value: 1},
				{Key: "createdAt", Value: -1},
			},
			Options: options.Index().SetName("approvals_status_created"),
		},
	}); err != nil {
		return err
	}
	_, err := s.audit.Indexes().CreateMany(ctx, []mongoDrv.IndexModel{
		{
			Keys:    bson.D{{Key: "expiresAt", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(0).SetName("audit_ttl"),
		},
		{
			Keys:    bson.D{{Key: "at", Value: -1}},
			Options: options.Index().SetName("audit_at"),
		},
		{
			// The audit screen filters by server and/or tool and sorts by at.
			Keys: bson.D{
				{Key: "server", Value: 1},
				{Key: "tool", Value: 1},
				{Key: "at", Value: -1},
			},
			Options: options.Index().SetName("audit_server_tool_at"),
		},
	})
	return err
}

// CreateApproval inserts a pending approval.
func (s *MongoStore) CreateApproval(ctx context.Context, a Approval) error {
	if a.ID == "" {
		return NewStoreError("approval create needs an id", false)
	}
	now := time.Now()
	if a.CreatedAt == 0 {
		a.CreatedAt = now.UnixMilli()
	}
	a.UpdatedAt = now.UnixMilli()
	if a.Status == "" {
		a.Status = StatusPending
	}
	if a.ExpiresAt.IsZero() {
		a.ExpiresAt = now.Add(ApprovalTTL)
	}
	if _, err := s.approvals.InsertOne(ctx, a); err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			// Ids are ObjectIDs, so this is a retry, not a collision. A line
			// still matters: "created" and "already existed" are different
			// facts to a triager reading one hold's story.
			s.logf("approvals: ignored duplicate create for %s", a.ID)
			return nil
		}
		return NewStoreError("approval create failed: "+err.Error(), true)
	}
	return nil
}

// GetApproval returns one approval; an expired row reads as NotFound.
func (s *MongoStore) GetApproval(ctx context.Context, id string) (Approval, error) {
	var a Approval
	if err := s.approvals.FindOne(ctx, bson.M{"_id": id}).Decode(&a); err != nil {
		if errors.Is(err, mongoDrv.ErrNoDocuments) {
			return Approval{}, &StoreError{Msg: "unknown approval " + id, NotFound: true}
		}
		return Approval{}, NewStoreError("approval read failed: "+err.Error(), true)
	}
	if !a.ExpiresAt.IsZero() && !a.ExpiresAt.After(time.Now()) {
		return Approval{}, &StoreError{Msg: "unknown approval " + id, NotFound: true}
	}
	return a.Clone(), nil
}

// DecideApproval flips a pending, unexpired approval terminal exactly once.
// Anything else races out: already decided, missing, or expired.
func (s *MongoStore) DecideApproval(ctx context.Context, id string, approved bool, by string, at time.Time) (Approval, error) {
	status := StatusDenied
	if approved {
		status = StatusApproved
	}
	res := s.approvals.FindOneAndUpdate(ctx,
		bson.M{"_id": id, "status": string(StatusPending), "expiresAt": bson.M{"$gt": at}},
		bson.M{"$set": bson.M{
			"status": status, "decidedBy": by,
			"decidedAt": at.UnixMilli(), "updatedAt": at.UnixMilli(),
		}},
		options.FindOneAndUpdate().SetReturnDocument(options.After))
	var a Approval
	if err := res.Decode(&a); err != nil {
		if errors.Is(err, mongoDrv.ErrNoDocuments) {
			// Missing reads 404; present-but-undecidable (decided or
			// expired) reads 409. One extra read, failure path only.
			var probe Approval
			if perr := s.approvals.FindOne(ctx, bson.M{"_id": id}).Decode(&probe); perr != nil {
				return Approval{}, &StoreError{Msg: "unknown approval " + id, NotFound: true}
			}
			return Approval{}, &StoreError{Msg: "approval " + id + " is not pending", Conflict: true}
		}
		return Approval{}, NewStoreError("approval decide failed: "+err.Error(), true)
	}
	return a.Clone(), nil
}

// TimeoutApproval flips a still-pending approval expired; decided rows keep
// their human verdict.
func (s *MongoStore) TimeoutApproval(ctx context.Context, id string, at time.Time) error {
	_, err := s.approvals.UpdateOne(ctx,
		bson.M{"_id": id, "status": string(StatusPending)},
		bson.M{"$set": bson.M{
			"status":    string(StatusExpired),
			"updatedAt": at.UnixMilli(), "decidedAt": at.UnixMilli(),
		}})
	if err != nil {
		return NewStoreError("approval timeout failed: "+err.Error(), true)
	}
	return nil
}

// ListApprovals returns matching approvals, newest first, capped.
func (s *MongoStore) ListApprovals(ctx context.Context, f ApprovalFilter) ([]Approval, error) {
	limit := f.Limit
	if limit <= 0 || limit > ListCap {
		limit = ListCap
	}
	filter := bson.M{}
	if f.Status != "" {
		filter["status"] = string(f.Status)
	}
	// TTL deletion lags (up to a minute), so an expired row would otherwise
	// list as pending and then 409 on decide. Filter on the same
	// expiresAt > now boundary GetApproval and DecideApproval use, and the
	// list contract matches the memory store's lazy drop.
	filter["expiresAt"] = bson.M{"$gt": time.Now()}
	cur, err := s.approvals.Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, NewStoreError("approval list failed: "+err.Error(), true)
	}
	defer cur.Close(ctx)
	out := make([]Approval, 0)
	skipped := 0
	for cur.Next(ctx) {
		var a Approval
		if err := cur.Decode(&a); err != nil {
			skipped++
			continue
		}
		out = append(out, a.Clone())
	}
	if skipped > 0 {
		s.logf("approvals: skipped %d undecodable row(s)", skipped)
	}
	if err := cur.Err(); err != nil {
		return nil, NewStoreError("approval list failed: "+err.Error(), true)
	}
	return out, nil
}

// Audit appends one audit row.
func (s *MongoStore) Audit(ctx context.Context, e AuditEntry) error {
	if e.ID == "" {
		return NewStoreError("audit needs an id", false)
	}
	if e.ExpiresAt.IsZero() {
		e.ExpiresAt = time.Now().Add(AuditTTL)
	}
	if _, err := s.audit.InsertOne(ctx, e); err != nil {
		return NewStoreError("audit write failed: "+err.Error(), true)
	}
	return nil
}

// ListAudit returns matching rows, newest first, capped.
func (s *MongoStore) ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error) {
	limit := f.Limit
	if limit <= 0 || limit > ListCap {
		limit = ListCap
	}
	filter := bson.M{}
	if f.Server != "" {
		filter["server"] = f.Server
	}
	if f.Tool != "" {
		filter["tool"] = f.Tool
	}
	cur, err := s.audit.Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "at", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, NewStoreError("audit list failed: "+err.Error(), true)
	}
	defer cur.Close(ctx)
	out := make([]AuditEntry, 0)
	skipped := 0
	for cur.Next(ctx) {
		var e AuditEntry
		if err := cur.Decode(&e); err != nil {
			skipped++
			continue
		}
		out = append(out, e)
	}
	if skipped > 0 {
		s.logf("approvals: skipped %d undecodable audit row(s)", skipped)
	}
	if err := cur.Err(); err != nil {
		return nil, NewStoreError("audit list failed: "+err.Error(), true)
	}
	return out, nil
}

// MarkInterrupted expires pending rows at startup and deletes expired ones,
// matching the memory backend: no "pending" row may outlive the relay that
// owned its wait.
func (s *MongoStore) MarkInterrupted(ctx context.Context, at time.Time) (int64, error) {
	if _, err := s.approvals.DeleteMany(ctx, bson.M{
		"status":    string(StatusPending),
		"expiresAt": bson.M{"$lte": at},
	}); err != nil {
		return 0, NewStoreError("delete expired approvals: "+err.Error(), true)
	}
	// createdAt < at, not just status=pending: a Create landing between the
	// delete and this update belongs to this startup, not the dead one, and
	// expiring it would kill a live hold's row.
	res, err := s.approvals.UpdateMany(ctx,
		bson.M{
			"status":    string(StatusPending),
			"createdAt": bson.M{"$lt": at.UnixMilli()},
		},
		bson.M{"$set": bson.M{
			"status":    string(StatusExpired),
			"updatedAt": at.UnixMilli(), "decidedAt": at.UnixMilli(),
		}})
	if err != nil {
		return 0, NewStoreError("mark interrupted: "+err.Error(), true)
	}
	return res.ModifiedCount, nil
}
