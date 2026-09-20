// Package money: MongoDB storage with transactional balance maintenance.
//
// Collections: money_accounts (name unique, never expires), money_transactions
// (90d TTL via expiresAt). Every mutation runs in a multi-document
// transaction writing doc(s) AND $inc balance delta(s) atomically.
package money

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agento/internal/mongo"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	accounts     = "money_accounts"
	transactions = "money_transactions"
	// RetentionDays drives the TTL target (tx date + window).
	RetentionDays = 90
)

var (
	dateRE       = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	accountTypes = []string{"cash", "bank", "card", "wallet", "other"}
	txTypes      = []string{"income", "expense", "transfer"}
)

// StoreError is the domain error: servers catch it and return {ok: False, error}.
type StoreError struct{ Msg string }

func (e *StoreError) Error() string { return e.Msg }

func fail(format string, args ...any) *StoreError {
	return &StoreError{Msg: fmt.Sprintf(format, args...)}
}

func expiry(day string, days int) primitive.DateTime {
	d, _ := time.Parse("2006-01-02", day)
	return primitive.NewDateTimeFromTime(d.AddDate(0, 0, days))
}

// idHex extracts hex from ObjectID or string id values.
func idHex(v any) string {
	if o, ok := v.(primitive.ObjectID); ok {
		return o.Hex()
	}
	return fmt.Sprint(v)
}

// signedDeltas maps account-id hex -> balance delta implied by a txn doc.
func signedDeltas(doc bson.M) map[string]float64 {
	amt := toFloat(doc["amount"])
	out := map[string]float64{}
	aid := idHex(doc["accountId"])
	switch doc["type"] {
	case "income":
		out[aid] = amt
	case "expense":
		out[aid] = -amt
	case "transfer":
		out[aid] = -amt
	dst := idHex(doc["sending_to"])
		out[dst] += amt
	}
	return out
}

// Store is the MongoDB backend; all balance writes go through transact.
type Store struct {
	client *mongoDrv.Client
	db     *mongoDrv.Database
	accts  *mongoDrv.Collection
	txns   *mongoDrv.Collection
	txOK   *bool // replica-set probe cache (only positive cached)
}

// New connects with an explicit URI/db (use FromEnv for single-root-.env).
func New(uri, dbName string) (*Store, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return nil, errors.New("MONGODB_URI is not set — MongoDB is the only backend")
	}
	if strings.TrimSpace(dbName) == "" {
		dbName = "hermes"
	}
	c, err := mongo.ConnectURI(uri)
	if err != nil {
		return nil, err
	}
	db := c.Database(dbName)
	return &Store{client: c, db: db, accts: db.Collection(accounts), txns: db.Collection(transactions)}, nil
}

// FromEnv builds a Store from MONGODB_URI/MONGODB_DB (single root .env).
func FromEnv() (*Store, error) {
	return New(os.Getenv("MONGODB_URI"), os.Getenv("MONGODB_DB"))
}

// useTransactions probes for replica-set support; only positive cached.
func (s *Store) useTransactions(ctx context.Context) bool {
	if s.txOK != nil && *s.txOK {
		return true
	}
	var hello bson.M
	if err := s.client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		return false
	}
	if name, _ := hello["setName"].(string); name != "" {
		t := true
		s.txOK = &t
		return true
	}
	if msg, _ := hello["msg"].(string); msg == "isdbgrid" {
		t := true
		s.txOK = &t
		return true
	}
	return false
}

// transact runs fn in a transaction with retry, or once without transaction
// semantics when the deployment has no replica set (plain context, no session).
func (s *Store) transact(ctx context.Context, fn func(ctx context.Context) error) error {
	if !s.useTransactions(ctx) {
		return fn(ctx)
	}
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
					return err // commit ambiguous — never re-run body
				}
			}
			if transient && attempt < 5 {
				continue
			}
		}
		return err
	}
}

// CreateAccount with unique name (index-backed); archived defaults false.
func (s *Store) CreateAccount(ctx context.Context, name, typ string, balance float64) (bson.M, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fail("account name is required")
	}
	if !contains(accountTypes, typ) {
		return nil, fail("account type must be one of %v", accountTypes)
	}
	doc := bson.M{"name": name, "type": typ, "balance": balance,
		"archived": false, "createdAt": primitive.NewDateTimeFromTime(time.Now().UTC())}
	res, err := s.accts.InsertOne(ctx, doc)
	if err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			return nil, fail("account '%s' already exists", name)
		}
		return nil, err
	}
	doc["_id"] = oidHex(res.InsertedID)
	return doc, nil
}

// ListAccounts alphabetical; archived hidden unless requested.
func (s *Store) ListAccounts(ctx context.Context, includeArchived bool) ([]bson.M, error) {
	filt := bson.M{}
	if !includeArchived {
		filt = bson.M{"archived": false}
	}
	cur, err := s.accts.Find(ctx, filt, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []bson.M{}
	}
	return rows, nil
}

// ArchiveAccount soft-deletes; history stays, writes blocked via resolve.
func (s *Store) ArchiveAccount(ctx context.Context, name string) (bool, error) {
	res, err := s.accts.UpdateOne(ctx, bson.M{"name": name}, bson.M{"$set": bson.M{"archived": true}})
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

func (s *Store) resolve(ctx context.Context, nameOrID string, forWrite bool) (bson.M, error) {
	q := bson.M{}
	if oid, err := primitive.ObjectIDFromHex(nameOrID); err == nil {
		q = bson.M{"_id": oid}
	} else {
		q = bson.M{"name": strings.TrimSpace(nameOrID)}
	}
	var acct bson.M
	if err := s.accts.FindOne(ctx, q).Decode(&acct); err != nil {
		return nil, fail("unknown account '%s'", nameOrID)
	}
	if forWrite {
		if arch, _ := acct["archived"].(bool); arch {
			return nil, fail("account '%v' is archived", acct["name"])
		}
	}
	return acct, nil
}

// Balances returns stored balances + total over active accounts.
func (s *Store) Balances(ctx context.Context) (bson.M, error) {
	cur, err := s.accts.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []bson.M{}
	}
	var total float64
	for _, r := range rows {
		if arch, _ := r["archived"].(bool); !arch {
			total += toFloat(r["balance"])
		}
	}
	return bson.M{"accounts": rows, "total": total}, nil
}

// Insert validates date/amount/type, then atomically writes doc + $inc deltas.
func (s *Store) Insert(ctx context.Context, date string, amount float64, typ, category, account, sendingTo, note, source string) (string, error) {
	if !dateRE.MatchString(date) {
		return "", fail("date must be YYYY-MM-DD")
	}
	if amount <= 0 {
		return "", fail("amount must be positive")
	}
	if !contains(txTypes, typ) {
		return "", fail("type must be one of %v", txTypes)
	}
	category = strings.ToLower(strings.TrimSpace(category))
	if category == "" {
		category = "other"
	}
	if !contains(Categories, category) {
		category = "other"
	}
	var tid string
	err := s.transact(ctx, func(sc context.Context) error {
		name := strings.TrimSpace(account)
		if name == "" {
			var existing []bson.M
			cur, err := s.accts.Find(sc, bson.M{"archived": false},
				options.Find().SetSort(bson.D{{Key: "name", Value: 1}}).SetProjection(bson.M{"name": 1}))
			if err != nil {
				return err
			}
			if err := cur.All(sc, &existing); err != nil {
				return err
			}
			if len(existing) == 0 {
				return fail("no accounts found — please configure an account first (use create_account)")
			}
			names := make([]string, 0, len(existing))
			for _, a := range existing {
				if n, _ := a["name"].(string); n != "" {
					names = append(names, n)
				}
			}
			return fail("%s", "account is required — available accounts: "+strings.Join(names, ", "))
		}
		src, err := s.resolve(sc, name, true)
		if err != nil {
			return err
		}
		if len(note) > 300 {
			note = note[:300]
		}
		if source == "" {
			source = "mcp"
		}
		if len(source) > 64 {
			source = source[:64]
		}
		doc := bson.M{"date": date, "amount": amount, "type": typ,
			"category": category, "note": note, "source": source,
			"accountId": src["_id"],
			"createdAt": primitive.NewDateTimeFromTime(time.Now().UTC()),
			"expiresAt": expiry(date, RetentionDays)}
		if typ == "transfer" {
			if strings.TrimSpace(sendingTo) == "" {
				return fail("transfer requires sending_to")
			}
			dst, err := s.resolve(sc, sendingTo, true)
			if err != nil {
				return err
			}
			if idHex(dst["_id"]) == idHex(src["_id"]) {
				return fail("transfer source and destination must differ")
			}
			doc["sending_to"] = dst["_id"]
		} else if strings.TrimSpace(sendingTo) != "" {
			return fail("sending_to is only valid for transfers")
		}
		res, err := s.txns.InsertOne(sc, doc)
		if err != nil {
			return err
		}
		for aid, delta := range signedDeltas(doc) {
			oid, err := primitive.ObjectIDFromHex(aid)
			if err != nil {
				return err
			}
			if _, err := s.accts.UpdateOne(sc, bson.M{"_id": oid},
				bson.M{"$inc": bson.M{"balance": delta}}); err != nil {
				return err
			}
		}
		tid = oidHex(res.InsertedID)
		return nil
	})
	return tid, err
}

// Delete by app-level filter; inverts balance deltas atomically.
func (s *Store) Delete(ctx context.Context, filt map[string]any) (int, error) {
	if len(filt) == 0 {
		return 0, fail("refusing to delete without a filter")
	}
	mongoFilt, err := s.toMongoFilter(ctx, filt)
	if err != nil {
		return 0, err
	}
	deleted := 0
	err = s.transact(ctx, func(sc context.Context) error {
		cur, err := s.txns.Find(sc, mongoFilt)
		if err != nil {
			return err
		}
		var matched []bson.M
		if err := cur.All(sc, &matched); err != nil {
			return err
		}
		if len(matched) == 0 {
			return nil
		}
		ids := make([]any, 0, len(matched))
		agg := map[string]float64{}
		for _, d := range matched {
			ids = append(ids, d["_id"])
			for aid, delta := range signedDeltas(d) {
				agg[aid] -= delta // inverse
			}
		}
		if _, err := s.txns.DeleteMany(sc, bson.M{"_id": bson.M{"$in": ids}}); err != nil {
			return err
		}
		for aid, delta := range agg {
			oid, err := primitive.ObjectIDFromHex(aid)
			if err != nil {
				return err
			}
			if _, err := s.accts.UpdateOne(sc, bson.M{"_id": oid},
				bson.M{"$inc": bson.M{"balance": delta}}); err != nil {
				return err
			}
		}
		deleted = len(matched)
		return nil
	})
	return deleted, err
}

// FixLast adjusts newest tx by delta; scales each leg so transfers stay balanced.
func (s *Store) FixLast(ctx context.Context, newAmount float64) (bool, error) {
	if newAmount <= 0 {
		return false, fail("amount must be positive")
	}
	fixed := false
	err := s.transact(ctx, func(sc context.Context) error {
		var last bson.M
		if err := s.txns.FindOne(sc, bson.M{},
			options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}})).Decode(&last); err != nil {
			if err == mongoDrv.ErrNoDocuments {
				return nil
			}
			return err
		}
		delta := newAmount - toFloat(last["amount"])
		if delta == 0 {
			fixed = true
			return nil
		}
		if _, err := s.txns.UpdateOne(sc, bson.M{"_id": last["_id"]},
			bson.M{"$set": bson.M{"amount": newAmount}}); err != nil {
			return err
		}
		last["amount"] = 1.0
		for aid, signed := range signedDeltas(last) {
			oid, err := primitive.ObjectIDFromHex(aid)
			if err != nil {
				return err
			}
			if _, err := s.accts.UpdateOne(sc, bson.M{"_id": oid},
				bson.M{"$inc": bson.M{"balance": signed * delta}}); err != nil {
				return err
			}
		}
		fixed = true
		return nil
	})
	return fixed, err
}

// Query date-range scan; account filter matches either side of transfers.
func (s *Store) Query(ctx context.Context, start, end string, typ, category, account *string) ([]bson.M, error) {
	filt := bson.M{"date": bson.M{"$gte": start, "$lte": end}}
	if typ != nil {
		filt["type"] = *typ
	}
	if category != nil {
		filt["category"] = *category
	}
	if account != nil {
		acct, err := s.findAccount(ctx, *account)
		if err != nil {
			return nil, err
		}
		filt["$or"] = []bson.M{{"accountId": acct["_id"]}, {"sending_to": acct["_id"]}}
	}
	cur, err := s.txns.Find(ctx, filt, options.Find().SetSort(bson.D{{Key: "date", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []bson.M{}
	}
	for _, r := range rows {
		r["accountId"] = idHex(r["accountId"])
		if st, ok := r["sending_to"]; ok {
			r["sending_to"] = idHex(st)
		}
	}
	return rows, nil
}

// Summarize income/expense/net + expense-only per-category ranking.
func (s *Store) Summarize(ctx context.Context, start, end string, account *string) (bson.M, error) {
	rows, err := s.Query(ctx, start, end, nil, nil, account)
	if err != nil {
		return nil, err
	}
	var income, expense float64
	byCat := map[string]float64{}
	for _, r := range rows {
		amt := toFloat(r["amount"])
		switch r["type"] {
		case "income":
			income += amt
		case "expense":
			expense += amt
			cat, _ := r["category"].(string)
			byCat[cat] += amt
		}
	}
	type kv struct {
		K string
		V float64
	}
	top := []kv{}
	for k, v := range byCat {
		top = append(top, kv{k, v})
	}
	for i := 0; i < len(top); i++ {
		for j := i + 1; j < len(top); j++ {
			if top[j].V > top[i].V {
				top[i], top[j] = top[j], top[i]
			}
		}
	}
	rank := make([]map[string]any, 0, len(top))
	for _, e := range top {
		rank = append(rank, map[string]any{"category": e.K, "total": e.V})
	}
	return bson.M{"income": income, "expense": expense, "net": income - expense,
		"count": len(rows), "by_category": rank}, nil
}

// Prune deletes old docs WITHOUT inverting balances (TTL parity).
func (s *Store) Prune(ctx context.Context, days int, dryRun bool) (int64, error) {
	if days <= 0 {
		days = RetentionDays
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	filt := bson.M{"date": bson.M{"$lt": cutoff}}
	if dryRun {
		return s.txns.CountDocuments(ctx, filt)
	}
	res, err := s.txns.DeleteMany(ctx, filt)
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

func (s *Store) findAccount(ctx context.Context, v string) (bson.M, error) {
	ors := []bson.M{{"name": v}}
	if oid, err := primitive.ObjectIDFromHex(v); err == nil {
		ors = append(ors, bson.M{"_id": oid})
	}
	var acct bson.M
	if err := s.accts.FindOne(ctx, bson.M{"$or": ors}).Decode(&acct); err != nil {
		return nil, fail("unknown account '%s'", v)
	}
	return acct, nil
}

func (s *Store) toMongoFilter(ctx context.Context, filt map[string]any) (bson.M, error) {
	out := bson.M{}
	for k, v := range filt {
		if k == "account" {
			vs, _ := v.(string)
			acct, err := s.findAccount(ctx, vs)
			if err != nil {
				return nil, err
			}
			out["$or"] = []bson.M{{"accountId": acct["_id"]}, {"sending_to": acct["_id"]}}
		} else {
			out[k] = v
		}
	}
	return out, nil
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	case primitive.Decimal128:
		f, _ := strconv.ParseFloat(n.String(), 64)
		return f
	default:
		return 0
	}
}

func oidHex(v any) string {
	if oid, ok := v.(primitive.ObjectID); ok {
		return oid.Hex()
	}
	return fmt.Sprint(v)
}

// EnsureSchema creates collections + indexes (idempotent) for tests/setup.
func (s *Store) EnsureSchema(ctx context.Context) error {
	for _, c := range []struct {
		name string
		keys bson.D
		opts *options.IndexOptions
	}{
		{accounts, bson.D{{Key: "name", Value: 1}}, options.Index().SetUnique(true).SetName("uniq_name")},
		{transactions, bson.D{{Key: "expiresAt", Value: 1}}, options.Index().SetName("ttl_expiresAt").SetExpireAfterSeconds(0)},
		{transactions, bson.D{{Key: "accountId", Value: 1}, {Key: "date", Value: 1}}, options.Index().SetName("acct_date")},
		{transactions, bson.D{{Key: "sending_to", Value: 1}, {Key: "date", Value: 1}}, options.Index().SetName("sendingto_date").SetSparse(true)},
		{transactions, bson.D{{Key: "date", Value: 1}, {Key: "type", Value: 1}}, options.Index().SetName("date_type")},
		{transactions, bson.D{{Key: "category", Value: 1}, {Key: "date", Value: 1}}, options.Index().SetName("cat_date")},
	} {
		if _, err := s.db.Collection(c.name).Indexes().CreateOne(ctx, mongoDrv.IndexModel{Keys: c.keys, Options: c.opts}); err != nil {
			return err
		}
	}
	return nil
}

