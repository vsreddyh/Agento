// Package money: MongoDB storage with transactional balance maintenance.
//
// Collections: money_accounts (name unique, never expires), money_transactions
// (90d TTL via expiresAt). Every mutation runs in a multi-document
// transaction writing doc(s) AND $inc balance delta(s) atomically.
package money

import (
	"agento/internal/mongostore"
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	// IdempotencyKeyMax caps the client key, in BYTES (what BSON and the unique
	// index measure), so it is a limit no caller can mistake for a character
	// count. A UUID is 36.
	IdempotencyKeyMax = 200
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

// expiry maps a YYYY-MM-DD date to its TTL instant: UTC midnight of that
// date, plus the retention window — i.e. about 05:30 IST on the target day,
// not IST midnight. So a row dated Sep 1 expires at 00:00 UTC Nov 30
// (≈05:30 IST), and an operator watching for it at IST midnight will wait
// five and a half hours. Documented wherever expiry surfaces.
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

// Paise keeps money exact: 1 rupee = 100 paise. The tool surface speaks
// rupees (float, unchanged contract); BSON stores int64 paise, so accumulated
// balances can never drift (0.1+0.2 stays 30 paise, not 0.30000000000000004).
// ToPaise rounds rupees to the nearest paise. It never validates: use
// ExactPaise at every entry point so a >2-decimal input is rejected rather
// than silently rounded.
func ToPaise(amount float64) int64 { return int64(math.Round(amount * 100)) }

// ExactPaise converts rupees to paise only when the value is exact to the
// paise — i.e. a round trip loses nothing. Sub-paise inputs (0.001) and
// >2-decimal inputs (0.005, 10.555) report false instead of being silently
// rounded: 0.005 must not become a 1-paise row.
func ExactPaise(amount float64) (int64, bool) {
	p := ToPaise(amount)
	if ToRupees(p) != amount {
		return 0, false
	}
	return p, true
}

// ToRupees renders stored paise for the tool surface. Whole rupees round-trip
// exactly; fractions use the shortest float repr (10.55, not dust).
func ToRupees(paise int64) float64 { return float64(paise) / 100 }

// paiseOf reads a stored amount/balance in paise, tolerating legacy doubles:
// pre-migration docs hold rupee floats, post-migration docs hold paise longs.
// After the EnsureSchema migration runs, only the int branches trigger.
func paiseOf(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int32:
		return int64(n)
	case int:
		return int64(n)
	case float64:
		return int64(math.Round(n * 100))
	case float32:
		return int64(math.Round(float64(n) * 100))
	case primitive.Decimal128:
		f, _ := strconv.ParseFloat(n.String(), 64)
		return int64(math.Round(f * 100))
	default:
		return 0
	}
}

// signedPaise maps account-id hex -> balance delta in paise implied by a txn
// doc. Integer math throughout: $inc stays int64 on int64 fields, so Mongo
// never silently re-floats a balance.
func signedPaise(doc bson.M) map[string]int64 {
	amt := paiseOf(doc["amount"])
	out := map[string]int64{}
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
	c, db, err := mongostore.Open(uri, dbName)
	if err != nil {
		return nil, err
	}
	s := &Store{client: c, db: db, accts: db.Collection(accounts), txns: db.Collection(transactions)}
	if err := s.EnsureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// FromEnv builds a Store from MONGODB_URI/MONGODB_DB (single root .env).
func FromEnv() (*Store, error) {
	uri, db := mongostore.Env()
	return New(uri, db)
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
//
// The name is trimmed and the type is lowercased+trimmed like transaction
// types, so `create type="Bank"` stores `bank` instead of rejecting what
// `log_transaction type="Bank"` accepts.
func (s *Store) CreateAccount(ctx context.Context, name, typ string, balance float64) (bson.M, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fail("account name is required")
	}
	typ = strings.ToLower(strings.TrimSpace(typ))
	if !contains(accountTypes, typ) {
		return nil, fail("account type must be one of %v", accountTypes)
	}
	// Exact paise like every other write: an opening balance of 0.005 is
	// rejected, not silently stored as 1 paise.
	balPaise, ok := ExactPaise(balance)
	if !ok {
		return nil, fail("balance %v is not an exact paise value — use at most 2 decimals", balance)
	}
	doc := bson.M{"name": name, "type": typ, "balance": balPaise,
		"archived": false, "createdAt": primitive.NewDateTimeFromTime(time.Now().UTC())}
	res, err := s.accts.InsertOne(ctx, doc)
	if err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			return nil, fail("account '%s' already exists", name)
		}
		return nil, err
	}
	doc["_id"] = oidHex(res.InsertedID)
	doc["balance"] = ToRupees(balPaise)
	return doc, nil
}

// ListAccounts alphabetical; archived hidden unless requested.
//
// `_id` is returned as a hex string. Transaction rows carry account ids, so
// without this the mapping id -> name is unknowable from the tool surface and
// a multi-account query result cannot be read.
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
	for _, r := range rows {
		r["_id"] = oidHex(r["_id"])
		// Stored paise, rendered rupees (see ToPaise).
		r["balance"] = ToRupees(paiseOf(r["balance"]))
	}
	return rows, nil
}

// ArchiveAccount soft-deletes; history stays, writes blocked via resolve.
// Only `log_transaction` is blocked: `delete` still inverts the
// archived account's balance by design (history stays mutable to the owner).
// The name is trimmed like every other account path (Create/resolve/findAccount/
// Insert), so `archive " Cash "` finds what `create " Cash "` stored as `Cash`.
// Accepts an id hex as well as a name (via findAccount), so an `_id` from
// `list_accounts` round-trips instead of reporting "unknown account". Returns
// the resolved account name (trimmed), never the raw input — so the caller can
// echo it without believing spaces/hex are the stored name.
func (s *Store) ArchiveAccount(ctx context.Context, name string) (string, bool, error) {
	acct, err := s.findAccount(ctx, name)
	if err != nil {
		if _, ok := err.(*StoreError); ok {
			return "", false, nil
		}
		return "", false, err
	}
	res, err := s.accts.UpdateOne(ctx, bson.M{"_id": acct["_id"]}, bson.M{"$set": bson.M{"archived": true}})
	if err != nil {
		return "", false, err
	}
	if res.MatchedCount == 0 {
		return "", false, nil
	}
	if n, _ := acct["name"].(string); n != "" {
		return n, true, nil
	}
	return strings.TrimSpace(name), true, nil
}

// UnarchiveAccount is the inverse of ArchiveAccount. Without it, archiving is a
// one-way door: the unique name index keeps the archived name, so the account
// can be neither restored nor re-created. Trimmed like ArchiveAccount so a
// padded name restores what it archived, and accepts an id hex for the same
// round-trip reason. Returns the resolved name, never the raw input.
func (s *Store) UnarchiveAccount(ctx context.Context, name string) (string, bool, error) {
	acct, err := s.findAccount(ctx, name)
	if err != nil {
		if _, ok := err.(*StoreError); ok {
			return "", false, nil
		}
		return "", false, err
	}
	res, err := s.accts.UpdateOne(ctx, bson.M{"_id": acct["_id"]}, bson.M{"$set": bson.M{"archived": false}})
	if err != nil {
		return "", false, err
	}
	if res.MatchedCount == 0 {
		return "", false, nil
	}
	if n, _ := acct["name"].(string); n != "" {
		return n, true, nil
	}
	return strings.TrimSpace(name), true, nil
}

func (s *Store) resolve(ctx context.Context, nameOrID string, forWrite bool) (bson.M, error) {
	// Trimmed before hex-parse like findAccount: without this, a padded
	// id-hex misses `_id` (hex-parse fails on spaces) and falls back to a
	// name lookup for the hex, so `sending_to="  <hex>  "` fails while the
	// same padding on `account` works. Both name and `_id` are tried via
	// `$or` (like findAccount), so an account literally named with 24 hex
	// chars resolves by name instead of missing as an unknown `_id` — the
	// old if/else preferred `_id` and made hex-shaped names unreachable via
	// `log`. The error quotes the trimmed value like every other path.
	nameOrID = strings.TrimSpace(nameOrID)
	ors := []bson.M{{"name": nameOrID}}
	if oid, err := primitive.ObjectIDFromHex(nameOrID); err == nil {
		ors = append(ors, bson.M{"_id": oid})
	}
	var acct bson.M
	if err := s.accts.FindOne(ctx, bson.M{"$or": ors}).Decode(&acct); err != nil {
		return nil, fail("unknown account '%s'", nameOrID)
	}
	if forWrite {
		if arch, _ := acct["archived"].(bool); arch {
			return nil, fail("account '%v' is archived", acct["name"])
		}
	}
	return acct, nil
}

// Balances returns stored balances + total over active accounts. The `accounts`
// list intentionally includes archived rows (unlike ListAccounts, which hides
// them by default) while `total` skips them — so `total` is not the sum of the
// listed balances once anything is archived. Documented on the tool + README.
// Balances are int64 paise in BSON, rendered as rupee floats here.
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
	var totalPaise int64
	for _, r := range rows {
		r["_id"] = oidHex(r["_id"])
		balPaise := paiseOf(r["balance"])
		r["balance"] = ToRupees(balPaise)
		if arch, _ := r["archived"].(bool); !arch {
			totalPaise += balPaise
		}
	}
	return bson.M{"accounts": rows, "total": ToRupees(totalPaise)}, nil
}

// Insert validates date/amount/type/category, then atomically writes doc + $inc
// deltas. It returns (id, created, error): created is false when idempotencyKey
// matched an existing row — that row's id comes back and nothing is written, so
// a retried call is a no-op instead of a second entry.
//
// A key must never be reused across distinct payloads: when the key already
// exists, the incoming date/amount/type/category/account/sending_to are compared
// against the stored row and a mismatch is rejected rather than returning the
// old row as a false "replay".
func (s *Store) Insert(ctx context.Context, date string, amount float64, typ, category, account, sendingTo, note, source, idempotencyKey string) (string, bool, error) {
	// Trimmed so writes agree with reads (`query_transactions` trims via
	// CheckDay): `date=" 2026-09-01 "` stores `2026-09-01` instead of failing
	// while the same string queries fine. Validated as a real calendar date,
	// not just the regex — `2026-02-30` matches YYYY-MM-DD but `time.Parse`
	// fails it, and storing it would set expiresAt to year-1 (immediate TTL
	// purge with the balance delta surviving, by design).
	date = strings.TrimSpace(date)
	if !dateRE.MatchString(date) {
		return "", false, fail("date must be YYYY-MM-DD")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return "", false, fail("date %q is not a real calendar date — use YYYY-MM-DD", date)
	}
	if amount <= 0 {
		return "", false, fail("amount must be positive")
	}
	// Exact paise only: a >2-decimal input is rejected, never silently
	// rounded (0.005 must not become a 1-paise row).
	paise, ok := ExactPaise(amount)
	if !ok {
		return "", false, fail("amount %v is not an exact paise value — use at most 2 decimals (min ₹0.01)", amount)
	}
	// Normalized the same way read-path filters are: a write of "Groceries"
	// and a read of "groceries" refer to the same category, and likewise for
	// type. Without this, `query_transactions type=Expense` silently matches
	// nothing logged as `expense`.
	typ = strings.ToLower(strings.TrimSpace(typ))
	if !contains(txTypes, typ) {
		return "", false, fail("type must be one of %v", txTypes)
	}
	category = strings.ToLower(strings.TrimSpace(category))
	if category == "" {
		category = "other"
	}
	// An unknown category is rejected rather than silently refiled under
	// "other": the caller must learn that the name it used does not exist.
	if !contains(Categories, category) {
		return "", false, fail("category %q does not exist — use one of %v", category, Categories)
	}
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	// Rejected, never truncated: a truncated key makes a 250-byte key
	// byte-identical to the 200-byte key that is its own prefix, so the unique
	// index reports a duplicate and a genuinely different request is handed the
	// first one's row as a "replay".
	if len(idempotencyKey) > IdempotencyKeyMax {
		return "", false, fail("idempotency_key is %d bytes; the limit is %d — send a shorter key (a UUID is 36)",
			len(idempotencyKey), IdempotencyKeyMax)
	}
	if idempotencyKey != "" {
		existing, found, err := s.byIdempotencyKey(ctx, idempotencyKey)
		if err != nil {
			return "", false, err
		}
		if found {
			if err := s.checkIdempotencyPayload(ctx, existing, idempotencyKey, date, paise, typ, category, account, sendingTo); err != nil {
				return "", false, err
			}
			return oidHex(existing["_id"]), false, nil
		}
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
		// Capped at 300/64 bytes and cut on a UTF-8 boundary, never mid-rune:
		// the caps are documented on the tool + README rather than rejected,
		// so a long note reads back truncated — but never as corrupt bytes.
		note = truncateBytes(note, 300)
		if source == "" {
			source = "mcp"
		}
		source = truncateBytes(source, 64)
		doc := bson.M{"date": date, "amount": paise, "type": typ,
			"category": category, "note": note, "source": source,
			"accountId": src["_id"],
			"createdAt": primitive.NewDateTimeFromTime(time.Now().UTC()),
			"expiresAt": expiry(date, RetentionDays)}
		// Omitted when empty so the sparse unique index ignores the document.
		if idempotencyKey != "" {
			doc["idempotencyKey"] = idempotencyKey
		}
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
		for aid, delta := range signedPaise(doc) {
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
	if err != nil {
		// Lost a race on the unique key: the row the other writer inserted is
		// the answer — but only when the payload matches. A different payload
		// under the same key is a caller bug, not a replay.
		if idempotencyKey != "" && mongoDrv.IsDuplicateKeyError(err) {
			if existing, found, rerr := s.byIdempotencyKey(ctx, idempotencyKey); rerr == nil && found {
				if cerr := s.checkIdempotencyPayload(ctx, existing, idempotencyKey, date, paise, typ, category, account, sendingTo); cerr != nil {
					return "", false, cerr
				}
				return oidHex(existing["_id"]), false, nil
			}
		}
		return "", false, err
	}
	return tid, true, nil
}

// byIdempotencyKey reports the transaction already carrying key, with the full
// row so the caller can verify the replay carries the same payload.
func (s *Store) byIdempotencyKey(ctx context.Context, key string) (bson.M, bool, error) {
	var doc bson.M
	err := s.txns.FindOne(ctx, bson.M{"idempotencyKey": key}).Decode(&doc)
	if err == mongoDrv.ErrNoDocuments {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return doc, true, nil
}

// checkIdempotencyPayload rejects a reused key carrying a different payload.
// Only the core semantic fields are compared (date/amount/type/category +
// both sides of a transfer); note/source are free-form annotations and are
// excluded so a retried call with a tweaked note is still a no-op.
func (s *Store) checkIdempotencyPayload(ctx context.Context, existing bson.M, key, date string, paise int64, typ, category, account, sendingTo string) error {
	mismatch := func(field string) error {
		return fail("idempotency_key %q was already used for a different transaction (mismatch in %s) — use a new key", key, field)
	}
	if fmt.Sprint(existing["date"]) != date {
		return mismatch("date")
	}
	// Integer compare: exact, unlike the old float equality.
	if paiseOf(existing["amount"]) != paise {
		return mismatch("amount")
	}
	if fmt.Sprint(existing["type"]) != typ {
		return mismatch("type")
	}
	if fmt.Sprint(existing["category"]) != category {
		return mismatch("category")
	}
	if strings.TrimSpace(account) == "" {
		return mismatch("account")
	}
	acct, err := s.findAccount(ctx, strings.TrimSpace(account))
	if err != nil {
		return mismatch("account")
	}
	if idHex(existing["accountId"]) != oidHex(acct["_id"]) {
		return mismatch("account")
	}
	incomingDst := strings.TrimSpace(sendingTo)
	_, hasExisting := existing["sending_to"]
	if typ == "transfer" {
		if incomingDst == "" {
			return mismatch("sending_to")
		}
		dst, err := s.findAccount(ctx, incomingDst)
		if err != nil {
			return mismatch("sending_to")
		}
		if !hasExisting {
			return mismatch("sending_to")
		}
		if idHex(existing["sending_to"]) != oidHex(dst["_id"]) {
			return mismatch("sending_to")
		}
	} else {
		if hasExisting || incomingDst != "" {
			return mismatch("sending_to")
		}
	}
	return nil
}

// Match returns the rows a delete filter would remove, without removing them —
// the dry-run half of delete_transactions.
func (s *Store) Match(ctx context.Context, filt map[string]any) ([]bson.M, error) {
	if len(filt) == 0 {
		return nil, fail("refusing to match without a filter")
	}
	mongoFilt, err := s.toMongoFilter(ctx, filt)
	if err != nil {
		return nil, err
	}
	// Same date+_id order as Query: a dry-run lists rows in the order a
	// delete would report and a query displays them.
	cur, err := s.txns.Find(ctx, mongoFilt, options.Find().SetSort(bson.D{{Key: "date", Value: 1}, {Key: "_id", Value: 1}}))
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
	names, err := s.accountNames(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		r["_id"] = oidHex(r["_id"])
		r["amount"] = ToRupees(paiseOf(r["amount"]))
		r["accountId"] = idHex(r["accountId"])
		r["account"] = names[idHex(r["accountId"])]
		if st, ok := r["sending_to"]; ok {
			r["sending_to"] = idHex(st)
			r["sending_to_account"] = names[idHex(st)]
		}
	}
	return rows, nil
}

// accountNames maps account-id hex -> name, for annotating transaction rows.
func (s *Store) accountNames(ctx context.Context) (map[string]string, error) {
	cur, err := s.accts.Find(ctx, bson.M{}, options.Find().SetProjection(bson.M{"name": 1}))
	if err != nil {
		return nil, err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, a := range rows {
		out[oidHex(a["_id"])] = fmt.Sprint(a["name"])
	}
	return out, nil
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
		agg := map[string]int64{}
		for _, d := range matched {
			ids = append(ids, d["_id"])
			for aid, delta := range signedPaise(d) {
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

// queryFilter builds the normalized Mongo filter both Query and Summarize
// share: date range plus optional type/category/account (either side of a
// transfer). Normalization and membership checks live here, once.
func (s *Store) queryFilter(ctx context.Context, start, end string, typ, category, account *string) (bson.M, error) {
	filt := bson.M{"date": bson.M{"$gte": start, "$lte": end}}
	if typ != nil {
		t := strings.ToLower(strings.TrimSpace(*typ))
		if !contains(txTypes, t) {
			return nil, fail("type %q does not exist — use one of %v", *typ, txTypes)
		}
		filt["type"] = t
	}
	if category != nil {
		c := strings.ToLower(strings.TrimSpace(*category))
		if !contains(Categories, c) {
			return nil, fail("category %q does not exist — use one of %v", *category, Categories)
		}
		filt["category"] = c
	}
	if account != nil {
		acct, err := s.findAccount(ctx, *account)
		if err != nil {
			return nil, err
		}
		filt["$or"] = []bson.M{{"accountId": acct["_id"]}, {"sending_to": acct["_id"]}}
	}
	return filt, nil
}

// Query date-range scan; account filter matches either side of transfers.
// Type/category filters are normalized exactly like writes (lowercase + trim),
// so `Groceries` finds what was logged as `groceries` — and unknown values are
// rejected like writes (and like unknown accounts), instead of silently
// returning zero rows for a typo.
//
// `_id` is returned as a hex string like Match does: without it, the documented
// `delete by id` workflow is unreachable from a query — the caller would hold a
// row with no id to delete it by.
//
// Pagination: limit==0 means no cap (direct store use only — the handler
// maps <=0 to 100 and caps >500 at 500, so MCP has no unlimited page); limits below zero are
// rejected like negative offsets, since an unbounded dump must never be the
// result of a typo. When limit>0, at most `limit` rows starting at `offset`;
// when limit==0, all rows from `offset` (no cap). `total`
// reporting the unpaged match count so the caller can tell a short page from
// the end. Sort key is date, then `_id` — deterministic order, but not a
// snapshot: a concurrent log or TTL expiry between page fetches can shift
// rows across page boundaries (dupes/skips on a live ledger). Negative
// offsets fail the same way (typo, not page 0).

func (s *Store) Query(ctx context.Context, start, end string, typ, category, account *string, limit, offset int) ([]bson.M, int64, error) {
	filt, err := s.queryFilter(ctx, start, end, typ, category, account)
	if err != nil {
		return nil, 0, err
	}
	// Rejected like the handler does: a negative skip is a typo, not page 0.
	// The handler checks first (tool-named error); this is defense for
	// direct store callers, who otherwise get a driver error. A negative
	// limit is the same class of typo (unlimited must be asked for with
	// exactly 0, never implied), so it fails here too.
	if offset < 0 {
		return nil, 0, fail("query_transactions offset must be >= 0 — got %d", offset)
	}
	if limit < 0 {
		return nil, 0, fail("query_transactions limit must be >= 0 — got %d (0 means no cap)", limit)
	}
	total, err := s.txns.CountDocuments(ctx, filt)
	if err != nil {
		return nil, 0, err
	}
	findOpts := options.Find().SetSort(bson.D{{Key: "date", Value: 1}, {Key: "_id", Value: 1}})
	if limit > 0 {
		findOpts.SetLimit(int64(limit))
	}
	// Skips apply with or without a limit: limit==0 means "no cap", not
	// "ignore the offset" — otherwise an offset page silently restarts.
	findOpts.SetSkip(int64(offset))
	cur, err := s.txns.Find(ctx, filt, findOpts)
	if err != nil {
		return nil, 0, err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, 0, err
	}
	if rows == nil {
		rows = []bson.M{}
	}
	// Rows carry account ids; the names come along so a multi-account result is
	// readable without a second lookup.
	names, err := s.accountNames(ctx)
	if err != nil {
		return nil, 0, err
	}
	for _, r := range rows {
		r["_id"] = oidHex(r["_id"])
		r["amount"] = ToRupees(paiseOf(r["amount"]))
		r["accountId"] = idHex(r["accountId"])
		r["account"] = names[idHex(r["accountId"])]
		if st, ok := r["sending_to"]; ok {
			r["sending_to"] = idHex(st)
			r["sending_to_account"] = names[idHex(st)]
		}
	}
	return rows, total, nil
}

// Summarize income/expense/net + expense-only per-category ranking. Transfers
// are counted in `count` but excluded from income/expense/net, and `by_category`
// ranks expenses only — so income categories never appear. Documented on the
// tool + README so `count` vs totals does not misread.
// Sums read stored paise directly (shared queryFilter, raw Find) — never the
// rendered rupee floats — so large totals stay integer-exact no matter how
// rendering evolves.
func (s *Store) Summarize(ctx context.Context, start, end string, account *string) (bson.M, error) {
	filt, err := s.queryFilter(ctx, start, end, nil, nil, account)
	if err != nil {
		return nil, err
	}
	cur, err := s.txns.Find(ctx, filt)
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
	var incomePaise, expensePaise int64
	byCat := map[string]int64{}
	for _, r := range rows {
		amt := paiseOf(r["amount"])
		switch r["type"] {
		case "income":
			incomePaise += amt
		case "expense":
			expensePaise += amt
			cat, _ := r["category"].(string)
			byCat[cat] += amt
		}
	}
	type kv struct {
		K string
		V int64
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
		rank = append(rank, map[string]any{"category": e.K, "total": ToRupees(e.V)})
	}
	return bson.M{"income": ToRupees(incomePaise), "expense": ToRupees(expensePaise), "net": ToRupees(incomePaise - expensePaise),
		"count": len(rows), "by_category": rank}, nil
}

// Prune deletes old docs WITHOUT inverting balances (TTL parity).
// The cutoff uses the same UTC frame as the TTL index it mirrors (expiresAt
// is a UTC instant): prune is a maintenance operation keyed to TTL's frame,
// not a user-visible date. Boundary note: prune keeps `date == cutoff`
// (`$lt`) while TTL expires that same row at 00:00 UTC today (plus monitor
// lag), so dry-run counts can trail TTL by a day — same frame, not same
// instant. User-visible dates (default log date, period phrases) are IST;
// instants and retention math are UTC.
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
	v = strings.TrimSpace(v)
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
	// Amounts are int64 paise here: ints pass through AS PAISE (10 means 10
	// paise, not ₹10 — the store's contract is paise-native since the int64
	// migration, and the MCP handler converts rupees before calling).
	// Rupee floats must be exact paise (round-trip check) or fail — a float
	// used to silently miss post-migration (stored 1010 vs filter 10.1),
	// the silent-zero-match class of bug. Non-numerics fail instead of
	// matching nothing.
	out := bson.M{}
	for k, v := range filt {
		switch k {
		case "amount":
			switch n := v.(type) {
			case int64:
				out[k] = n
			case int32:
				out[k] = int64(n)
			case int:
				out[k] = int64(n)
			case float64:
				p, ok := ExactPaise(n)
				if !ok {
					return nil, fail("amount %v is not an exact paise value — use at most 2 decimals", n)
				}
				out[k] = p
			case float32:
				f := float64(n)
				p, ok := ExactPaise(f)
				if !ok {
					return nil, fail("amount %v is not an exact paise value — use at most 2 decimals", n)
				}
				out[k] = p
			default:
				return nil, fail("amount must be a number (int64 paise, or an exact-paise float)")
			}
		case "account":
			vs, _ := v.(string)
			acct, err := s.findAccount(ctx, vs)
			if err != nil {
				return nil, err
			}
			out["$or"] = []bson.M{{"accountId": acct["_id"]}, {"sending_to": acct["_id"]}}
		case "id":
			vs, _ := v.(string)
			oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(vs))
			if err != nil {
				return nil, fail("'%s' is not a transaction id", vs)
			}
			out["_id"] = oid
		case "type", "category":
			// Normalized like writes: `Groceries` deletes what was logged as
			// `groceries` instead of reporting "would_delete: 0" for rows that
			// exist — and unknown values are rejected like writes (and like
			// unknown accounts), instead of silently matching nothing for a
			// typo. Non-strings pass through for the driver to reject.
			if vs, ok := v.(string); ok {
				norm := strings.ToLower(strings.TrimSpace(vs))
				if k == "type" {
					if !contains(txTypes, norm) {
						return nil, fail("type %q does not exist — use one of %v", vs, txTypes)
					}
				} else if !contains(Categories, norm) {
					return nil, fail("category %q does not exist — use one of %v", vs, Categories)
				}
				out[k] = norm
			} else {
				out[k] = v
			}
		case "date":
			// Trimmed like writes/reads so a padded delete filter finds what
			// `log_transaction` stored trimmed instead of reporting
			// `would_delete: 0`. Calendar validity is enforced in the handler
			// via CheckDay (which rejects); the store stays a trim-only
			// defense for direct callers.
			if vs, ok := v.(string); ok {
				out[k] = strings.TrimSpace(vs)
			} else {
				out[k] = v
			}
		default:
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

// truncateBytes caps s at n bytes without splitting a UTF-8 rune: a naive
// s[:n] can cut a multi-byte character mid-sequence and store invalid bytes.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func oidHex(v any) string {
	if oid, ok := v.(primitive.ObjectID); ok {
		return oid.Hex()
	}
	return fmt.Sprint(v)
}

// migrateFloatMoney converts legacy rupee numbers to int64 paise, once,
// idempotently: only double/decimal documents match, so post-migration runs
// (all longs) and fresh databases find nothing to do. int32/int64 are
// deliberately excluded — new code writes those as paise already, so an old
// int can only mean paise, never legacy rupees. Decimal128 never shipped from
// this codebase, but a stray one converts exactly like a double rather than
// being left behind as the sole decimal in a long world.
// Rounding heals old float dust (99.9999999999 rupees → 10000 paise) instead
// of preserving it. Sub-paise dust (0.001–0.004, allowed by the old
// `amount <= 0` check) floors to the 1-paise minimum on transactions: a 0
// amount would violate the new >0 invariant and be undeletable by amount
// (the tool rejects 0 as "absent"). Balances keep a floored 0 — an empty
// account legitimately holds nothing.
// The whole migration runs inside one transaction (see transact): a
// read-then-$set loop racing a concurrent balance $inc could otherwise
// clobber the increment on first boot after deploy.
func (s *Store) migrateFloatMoney(ctx context.Context) error {
	return s.transact(ctx, func(sc context.Context) error {
		for _, m := range []struct {
			coll     *mongoDrv.Collection
			field    string
			floorOne bool // transactions: no 0-paise rows may exist
		}{{s.accts, "balance", false}, {s.txns, "amount", true}} {
			cur, err := m.coll.Find(sc, bson.M{m.field: bson.M{"$type": []string{"double", "decimal"}}})
			if err != nil {
				return err
			}
			var rows []bson.M
			if err := cur.All(sc, &rows); err != nil {
				return err
			}
			for _, r := range rows {
				paise := paiseOf(r[m.field])
				if m.floorOne && paise < 1 {
					paise = 1
				}
				if _, err := m.coll.UpdateOne(sc, bson.M{"_id": r["_id"]},
					bson.M{"$set": bson.M{m.field: paise}}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// EnsureSchema creates collections + indexes (idempotent). Indexes come
// first: if the data migration below fails transiently, the TTL/unique
// guarantees are already in place for the restart. Called by New so fresh
// databases get the unique-name and TTL indexes immediately.
// Also migrates legacy float rupees to int64 paise (see ToPaise) — new code
// writes longs only, so the migration converges on first boot.
func (s *Store) EnsureSchema(ctx context.Context) error {
	for _, c := range []struct {
		name string
		keys bson.D
		opts *options.IndexOptions
	}{
		{accounts, bson.D{{Key: "name", Value: 1}}, options.Index().SetUnique(true).SetName("uniq_name")},
		{transactions, bson.D{{Key: "expiresAt", Value: 1}}, options.Index().SetName("ttl_expiresAt").SetExpireAfterSeconds(0)},
		{transactions, bson.D{{Key: "idempotencyKey", Value: 1}},
			options.Index().SetName("uniq_idempotency").SetUnique(true).SetSparse(true)},
		{transactions, bson.D{{Key: "accountId", Value: 1}, {Key: "date", Value: 1}}, options.Index().SetName("acct_date")},
		{transactions, bson.D{{Key: "sending_to", Value: 1}, {Key: "date", Value: 1}}, options.Index().SetName("sendingto_date").SetSparse(true)},
		{transactions, bson.D{{Key: "date", Value: 1}, {Key: "type", Value: 1}}, options.Index().SetName("date_type")},
		{transactions, bson.D{{Key: "category", Value: 1}, {Key: "date", Value: 1}}, options.Index().SetName("cat_date")},
	} {
		if _, err := s.db.Collection(c.name).Indexes().CreateOne(ctx, mongoDrv.IndexModel{Keys: c.keys, Options: c.opts}); err != nil {
			return err
		}
	}
	return s.migrateFloatMoney(ctx)
}
