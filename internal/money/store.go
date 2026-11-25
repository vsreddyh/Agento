// Package money: MongoDB storage with derived balances.
//
// Collections: money_accounts + money_transactions + money_budgets +
// money_categories — all permanent. Rows never expire: the only way a row disappears is a
// user-initiated delete. There is no stored running total: an account's
// balance is its opening seed (changeable only via edit_account) plus the
// signed sum of its ledger legs, computed on read — so a balance can never
// drift from its rows.
package money

import (
	"agento/internal/mongostore"
	"context"
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

// idHex extracts hex from ObjectID or string id values.
func idHex(v any) string {
	if o, ok := v.(primitive.ObjectID); ok {
		return o.Hex()
	}
	return fmt.Sprint(v)
}

// Paise keeps money exact: 1 rupee = 100 paise. The tool surface speaks
// paise (int64), matching BSON storage exactly, so accumulated balances can
// never drift (0.1+0.2 stays 30 paise, not 0.30000000000000004) and no float
// crosses the MCP boundary. ToPaise rounds rupees to the nearest paise for
// the paths that still accept them (delete filters from direct store
// callers); it never validates — ExactPaise is the gate that rejects
// sub-paise input instead of silently rounding it.
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

// ToRupees renders stored paise as rupees. Whole rupees round-trip
// exactly; fractions use the shortest float repr (10.55, not dust).
// Only legacy-migration and tolerance paths still need it — the tool
// surface reports paise integers.
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
// doc. Integer math throughout: legs sum as int64 paise, so rendering never
// re-floats a balance (0.1+0.2 stays 30 paise, not 0.30000000000000004).
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

// Store is the MongoDB backend. Writes are single documents — balances are
// derived from the ledger, never stored — so there is nothing to transact:
// no multi-document write exists that could half-land.
type Store struct {
	db      *mongoDrv.Database
	accts   *mongoDrv.Collection
	txns    *mongoDrv.Collection
	budgets *mongoDrv.Collection
	cats    *mongoDrv.Collection
}

// New connects with an explicit URI/db (use FromEnv for single-root-.env).
func New(uri, dbName string) (*Store, error) {
	_, db, err := mongostore.Open(uri, dbName)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, accts: db.Collection(accounts), txns: db.Collection(transactions), budgets: db.Collection(budgets), cats: db.Collection(categories)}
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

// CreateAccount with unique name (index-backed); archived defaults false.
//
// The name is trimmed and the type is lowercased+trimmed like transaction
// types, so `create type="Bank"` stores `bank` instead of rejecting what
// `log_transaction type="Bank"` accepts.
//
// The opening balance is stored once, as an immutable-by-writes int64-paise
// seed (`openingBalance`): it is added to the derived leg sums on every read,
// and logged rows never touch it — only edit_account changes it, by delta,
// with no transaction row. The tool surface speaks paise (int64), matching
// storage exactly — no float crosses the MCP boundary.
func (s *Store) CreateAccount(ctx context.Context, name, typ string, balancePaise int64) (bson.M, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fail("account name is required")
	}
	typ = strings.ToLower(strings.TrimSpace(typ))
	if !contains(accountTypes, typ) {
		return nil, fail("account type must be one of %v", accountTypes)
	}
	openingPaise := balancePaise
	doc := bson.M{"name": name, "type": typ, "openingBalance": openingPaise,
		"archived": false, "createdAt": primitive.NewDateTimeFromTime(time.Now().UTC())}
	res, err := s.accts.InsertOne(ctx, doc)
	if err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			return nil, fail("account '%s' already exists", name)
		}
		return nil, err
	}
	doc["_id"] = oidHex(res.InsertedID)
	// Echo the derived total under the same `balance_paise` key
	// get_balances uses — not the storage field. A new account has no legs,
	// so the total is exactly the seed, but callers should never see
	// `openingBalance`.
	delete(doc, "openingBalance")
	doc["balance_paise"] = openingPaise
	return doc, nil
}

// derivedLegs sums stored transaction legs per account-id hex, in int64
// paise. Balances are derived from the ledger, never stored — and only from
// live rows: soft-deleted rows (deletedAt set, awaiting the 10-day TTL) are
// excluded, so a delete moves the totals the moment it flags.
func (s *Store) derivedLegs(ctx context.Context) (map[string]int64, error) {
	cur, err := s.txns.Find(ctx, liveFilter())
	if err != nil {
		return nil, err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	agg := map[string]int64{}
	for _, r := range rows {
		for aid, delta := range signedPaise(r) {
			agg[aid] += delta
		}
	}
	return agg, nil
}

// ListAccounts alphabetical; archived hidden unless requested.
//
// `_id` is returned as a hex string. Transaction rows carry account ids, so
// without this the mapping id -> name is unknowable from the tool surface and
// a multi-account query result cannot be read.
//
// `balance_paise` is opening seed + derived legs, as int64 paise.
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
	legs, err := s.derivedLegs(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		id := idHex(r["_id"])
		r["_id"] = oidHex(r["_id"])
		// Opening seed plus signed legs; a missing seed (pre-derivation row
		// that skipped migration, which should not exist) reads as 0.
		// The storage field never leaves the store — see create, same rule.
		r["balance_paise"] = paiseOf(r["openingBalance"]) + legs[id]
		delete(r, "openingBalance")
	}
	return rows, nil
}

// ArchiveAccount soft-deletes; history stays, writes blocked via resolve.
// Only a NEW write is blocked: `log_transaction` rejects an archived account,
// while `delete_transaction` and `edit_transaction` still apply to rows on
// an archived account by design (history stays mutable to the owner). Editing a
// row that already sits on an archived leg is therefore fine too — including
// resupplying that leg, since the gate is on moving a leg TO an archived
// account, not on naming the one already there.
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

// EditAccount renames an account, changes its type, and/or corrects its
// opening-balance seed — the three things archiving cannot fix.
// Name, type and seed are the only mutable account fields; at least one must
// be set. A rename keeps every transaction row (rows reference the account
// id, never the name); a colliding name is rejected. A seed correction moves
// the derived balance by exactly the delta and writes no transaction row —
// unlike a fudged adjustment entry, it leaves income/expense untouched. The
// type is normalized like CreateAccount. Works on archived accounts too:
// fixing a typo'd archived account must not require unarchiving it first.
func (s *Store) EditAccount(ctx context.Context, idOrName string, newName *string, newType *string, openingPaise *int64) (bson.M, error) {
	acct, err := s.findAccount(ctx, idOrName)
	if err != nil {
		return nil, err
	}
	oid, ok := acct["_id"].(primitive.ObjectID)
	if !ok {
		return nil, fail("account '%s' has no id", strings.TrimSpace(idOrName))
	}
	set := bson.M{}
	if newName != nil {
		n := strings.TrimSpace(*newName)
		if n == "" {
			return nil, fail("edit_account name must not be blank")
		}
		if n != fmt.Sprint(acct["name"]) {
			var clash bson.M
			if cerr := s.accts.FindOne(ctx, bson.M{"name": n}).Decode(&clash); cerr == nil {
				return nil, fail("account '%s' already exists", n)
			} else if cerr != mongoDrv.ErrNoDocuments {
				return nil, cerr
			}
			set["name"] = n
		}
	}
	if newType != nil {
		t := strings.ToLower(strings.TrimSpace(*newType))
		if !contains(accountTypes, t) {
			return nil, fail("account type must be one of %v", accountTypes)
		}
		if t != fmt.Sprint(acct["type"]) {
			set["type"] = t
		}
	}
	if openingPaise != nil {
		if *openingPaise != paiseOf(acct["openingBalance"]) {
			set["openingBalance"] = *openingPaise
		}
	}
	if len(set) == 0 {
		return nil, fail("edit_account changes nothing — provide a different name, type or opening balance")
	}
	if _, err := s.accts.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": set}); err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			return nil, fail("account '%s' already exists", strings.TrimSpace(*newName))
		}
		return nil, err
	}
	var stored bson.M
	if err := s.accts.FindOne(ctx, bson.M{"_id": oid}).Decode(&stored); err != nil {
		return nil, err
	}
	legs, err := s.derivedLegs(ctx)
	if err != nil {
		return nil, err
	}
	id := idHex(stored["_id"])
	stored["_id"] = oidHex(stored["_id"])
	stored["balance_paise"] = paiseOf(stored["openingBalance"]) + legs[id]
	delete(stored, "openingBalance")
	return stored, nil
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

// Balances returns derived balances + total over active accounts. The `accounts`
// list intentionally includes archived rows (unlike ListAccounts, which hides
// them by default) while `total` skips them — so `total` is not the sum of the
// listed balances once anything is archived. Documented on the tool + README.
// Balances are opening seed + signed legs in int64 paise, reported as
// balance_paise integers.
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
	legs, err := s.derivedLegs(ctx)
	if err != nil {
		return nil, err
	}
	var totalPaise int64
	for _, r := range rows {
		id := idHex(r["_id"])
		r["_id"] = oidHex(r["_id"])
		balPaise := paiseOf(r["openingBalance"]) + legs[id]
		r["balance_paise"] = balPaise
		delete(r, "openingBalance")
		if arch, _ := r["archived"].(bool); !arch {
			totalPaise += balPaise
		}
	}
	return bson.M{"accounts": rows, "total_paise": totalPaise}, nil
}

// Insert validates date/amount/type/category, then writes the doc. It returns (id, created, error): created is false when idempotencyKey
// matched an existing row — that row's id comes back and nothing is written, so
// a retried call is a no-op instead of a second entry.
//
// A key must never be reused across distinct payloads: when the key already
// exists, the incoming date/amount/type/category/account/sending_to are compared
// against the stored row and a mismatch is rejected rather than returning the
// old row as a false "replay".
func (s *Store) Insert(ctx context.Context, date string, amountPaise int64, typ, category, account, sendingTo, note, source, idempotencyKey string) (string, bool, error) {
	// Trimmed so writes agree with reads (`query_transactions` trims via
	// CheckDay): `date=" 2026-09-01 "` stores `2026-09-01` instead of failing
	// while the same string queries fine. Validated as a real calendar date,
	// not just the regex — `2026-02-30` matches YYYY-MM-DD but `time.Parse`
	// fails it.
	date = strings.TrimSpace(date)
	if !dateRE.MatchString(date) {
		return "", false, fail("date must be YYYY-MM-DD")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return "", false, fail("date %q is not a real calendar date — use YYYY-MM-DD", date)
	}
	if amountPaise <= 0 {
		return "", false, fail("amount_paise must be positive")
	}
	paise := amountPaise
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
	// The live set is user-managed (money_categories), not a fixed list.
	var err error
	if category, err = s.requireCategory(ctx, "log_transaction", category); err != nil {
		return "", false, err
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
	name := strings.TrimSpace(account)
	if name == "" {
		var existing []bson.M
		cur, err := s.accts.Find(ctx, bson.M{"archived": false},
			options.Find().SetSort(bson.D{{Key: "name", Value: 1}}).SetProjection(bson.M{"name": 1}))
		if err != nil {
			return "", false, err
		}
		if err := cur.All(ctx, &existing); err != nil {
			return "", false, err
		}
		if len(existing) == 0 {
			return "", false, fail("no accounts found — please configure an account first (use create_account)")
		}
		names := make([]string, 0, len(existing))
		for _, a := range existing {
			if n, _ := a["name"].(string); n != "" {
				names = append(names, n)
			}
		}
		return "", false, fail("%s", "account is required — available accounts: "+strings.Join(names, ", "))
	}
	src, err := s.resolve(ctx, name, true)
	if err != nil {
		return "", false, err
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
		"createdAt": primitive.NewDateTimeFromTime(time.Now().UTC())}
	// Omitted when empty so the sparse unique index ignores the document.
	if idempotencyKey != "" {
		doc["idempotencyKey"] = idempotencyKey
	}
	if typ == "transfer" {
		if strings.TrimSpace(sendingTo) == "" {
			return "", false, fail("transfer requires sending_to")
		}
		dst, err := s.resolve(ctx, sendingTo, true)
		if err != nil {
			return "", false, err
		}
		if idHex(dst["_id"]) == idHex(src["_id"]) {
			return "", false, fail("transfer source and destination must differ")
		}
		doc["sending_to"] = dst["_id"]
	} else if strings.TrimSpace(sendingTo) != "" {
		return "", false, fail("sending_to is only valid for transfers")
	}
	res, err := s.txns.InsertOne(ctx, doc)
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
	return oidHex(res.InsertedID), true, nil
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
	// A replay of a soft-deleted row is rejected, not returned as a no-op:
	// the id it would hand back is invisible in every read until restored,
	// so a silent "success" would read as a write that never landed.
	if _, flagged := existing["deletedAt"]; flagged {
		return fail("idempotency_key %q was already used for a transaction that has since been deleted — restore it with restore_transactions to replay", key)
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
		r["amount_paise"] = paiseOf(r["amount"])
		delete(r, "amount")
		r["accountId"] = idHex(r["accountId"])
		r["account"] = names[idHex(r["accountId"])]
		if st, ok := r["sending_to"]; ok {
			r["sending_to"] = idHex(st)
			r["sending_to_account"] = names[idHex(st)]
		}
	}
	return rows, nil
}

// liveFilter excludes soft-deleted rows from the ledger read paths: Match,
// Query, window scans, derived legs and balances all build on it, so a
// flagged row vanishes from every aggregate the moment it is flagged and
// returns only via restore — until the 10-day TTL removes it for good. The
// by-id verbs (edit, restore, delete-by-id, idempotency replay) read the
// flag explicitly to reject or restore instead of matching through it.
func liveFilter() bson.M {
	return bson.M{"deletedAt": bson.M{"$exists": false}}
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

// Delete flags matching rows with deletedAt instead of removing them: the
// rows vanish from every read path the moment they flag (all reads build on
// liveFilter) and the 10-day TTL index removes them for good. A second delete
// of an already-flagged row matches nothing — like an unknown id, it reports
// 0 rather than deleting twice. Balances need no adjustment: they are derived
// from the surviving rows, so the total follows automatically.
func (s *Store) Delete(ctx context.Context, filt map[string]any) (int, error) {
	if len(filt) == 0 {
		return 0, fail("refusing to delete without a filter")
	}
	mongoFilt, err := s.toMongoFilter(ctx, filt)
	if err != nil {
		return 0, err
	}
	res, err := s.txns.UpdateMany(ctx, mongoFilt,
		bson.M{"$set": bson.M{"deletedAt": primitive.NewDateTimeFromTime(time.Now().UTC())}})
	if err != nil {
		return 0, err
	}
	return int(res.ModifiedCount), nil
}

// CheckID resolves one transaction id for the by-id verbs: malformed ids
// fail, unknown ids fail like everywhere else, and flagged (soft-deleted)
// ids fail as already-deleted — naming restore_transactions instead of
// reading as a typo'd unknown id. Live rows return deleted=false.
func (s *Store) CheckID(ctx context.Context, id string) (bool, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return false, fail("'%s' is not a transaction id", id)
	}
	var existing bson.M
	if err := s.txns.FindOne(ctx, bson.M{"_id": oid}).Decode(&existing); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return false, fail("unknown transaction id '%s'", strings.TrimSpace(id))
		}
		return false, err
	}
	if _, flagged := existing["deletedAt"]; flagged {
		return true, fail("transaction id '%s' is already deleted — restore it with restore_transactions to see it again", strings.TrimSpace(id))
	}
	return false, nil
}

// RestoreTransaction clears deletedAt on one flagged row, returning it to
// every read path with no other write. An unknown id fails like delete and
// edit by id; a live (never-flagged) row fails as not deleted rather than
// reporting a no-op success.
func (s *Store) RestoreTransaction(ctx context.Context, id string) (bson.M, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("'%s' is not a transaction id", id)
	}
	var existing bson.M
	if err := s.txns.FindOne(ctx, bson.M{"_id": oid}).Decode(&existing); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, fail("unknown transaction id '%s'", strings.TrimSpace(id))
		}
		return nil, err
	}
	if _, flagged := existing["deletedAt"]; !flagged {
		return nil, fail("transaction id '%s' is not deleted", strings.TrimSpace(id))
	}
	if _, err := s.txns.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$unset": bson.M{"deletedAt": ""}}); err != nil {
		return nil, err
	}
	rows, err := s.Match(ctx, map[string]any{"id": strings.TrimSpace(id)})
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 {
		return nil, fail("restored row %s reads back %d rows", strings.TrimSpace(id), len(rows))
	}
	return rows[0], nil
}

// EditTransaction updates one row by id; the row changes in place and
// balances follow from the ledger, so no separate rebalance step exists.
// Nil means leave the field alone (the handler passes nil for absent or
// whitespace-only inputs, like delete filters) — except converting
// a row INTO a transfer, which still requires sending_to; at least one field
// must be supplied (a same-value supply succeeds with changed:false). New
// values validate exactly like Insert: positive int64-paise amounts, known
// type/category, resolved accounts, transfer rules, real calendar
// dates. A type/account/amount move changes the row; derived balances follow,
// so a move can never half-land — there is no second write to fail. A date
// change just moves the row's date — rows never expire, so any past date
// stays put. Note is truncated like a write. Moving a leg TO an archived
// account rejects, like a new log; resupplying the leg already on the row
// does not (it is not a move, so the read-then-edit flow keeps working —
// including converting AWAY from a transfer, where the resupplied destination
// is ignored and the leg drops).
// Archived legs stay editable in place like deletes do.
func (s *Store) EditTransaction(ctx context.Context, id string, amountPaise *int64, typ, category, account, sendingTo, date, note *string) (bool, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return false, fail("'%s' is not a transaction id", id)
	}
	changed := false
	var old bson.M
	if err := s.txns.FindOne(ctx, bson.M{"_id": oid}).Decode(&old); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return false, fail("unknown transaction id '%s'", strings.TrimSpace(id))
		}
		return false, err
	}
	// Flagged rows are invisible everywhere else: editing one must fail
	// upfront like restore's guards do, not succeed-and-stay-flagged (the
	// handler's readback would then report 0 rows as a fake glitch).
	if _, flagged := old["deletedAt"]; flagged {
		return false, fail("transaction id '%s' is deleted — restore it with restore_transactions first", strings.TrimSpace(id))
	}
	newPaise := paiseOf(old["amount"])
	newType := fmt.Sprint(old["type"])
	newCat, _ := old["category"].(string)
	newDate := fmt.Sprint(old["date"])
	newNote, _ := old["note"].(string)
	newAcctOID, _ := old["accountId"].(primitive.ObjectID)
	newDstOID, hadDst := old["sending_to"].(primitive.ObjectID)
	if amountPaise != nil {
		if *amountPaise <= 0 {
			return false, fail("amount_paise must be positive")
		}
		p := *amountPaise
		if p != newPaise {
			changed = true
		}
		newPaise = p
	}
	if typ != nil {
		t := strings.ToLower(strings.TrimSpace(*typ))
		if !contains(txTypes, t) {
			return false, fail("type must be one of %v", txTypes)
		}
		if t != newType {
			changed = true
		}
		newType = t
	}
	if category != nil {
		c, err := s.requireCategory(ctx, "edit_transaction", *category)
		if err != nil {
			return false, err
		}
		if c != newCat {
			changed = true
		}
		newCat = c
	}
	if account != nil {
		// Resolved without the write gate first: resupplying the leg the
		// row already sits on is not a move, so an archived account only
		// rejects when the edit actually moves the leg elsewhere — the
		// read-then-edit flow (copy `account` from `query_transactions`
		// back into `edit_transaction`) must not fail on exactly the rows
		// the docs say stay editable.
		acct, err := s.resolve(ctx, *account, false)
		if err != nil {
			return false, err
		}
		aoid, ok := acct["_id"].(primitive.ObjectID)
		if !ok {
			return false, fail("unknown account '%s'", strings.TrimSpace(*account))
		}
		if aoid != newAcctOID {
			if arch, _ := acct["archived"].(bool); arch {
				return false, fail("account '%v' is archived", acct["name"])
			}
			changed = true
		}
		newAcctOID = aoid
	}
	wantDst := sendingTo != nil
	if newType == "transfer" {
		if sendingTo != nil {
			// Same rule as `account` above: naming the destination leg
			// already on the row is not a move, so it never rejects —
			// only moving TO an archived account does.
			dst, err := s.resolve(ctx, *sendingTo, false)
			if err != nil {
				return false, err
			}
			doid, ok := dst["_id"].(primitive.ObjectID)
			if !ok {
				return false, fail("unknown account '%s'", strings.TrimSpace(*sendingTo))
			}
			if !hadDst || doid != newDstOID {
				if arch, _ := dst["archived"].(bool); arch {
					return false, fail("account '%v' is archived", dst["name"])
				}
				changed = true
			}
			newDstOID, hadDst = doid, true
		} else if !hadDst {
			return false, fail("transfer requires sending_to")
		}
		if newDstOID == newAcctOID {
			return false, fail("transfer source and destination must differ")
		}
	} else if wantDst {
		// Converting away from a transfer while resupplying the leg
		// already on the row is not a contradiction: it is the
		// read-then-edit flow copying both legs back before fixing the
		// type. Same OID means "the old leg", so it is ignored — the
		// drop below still applies. A different destination, or any
		// sending_to on a row that never had one, is still rejected.
		if !hadDst {
			return false, fail("sending_to is only valid for transfers")
		}
		dst, err := s.resolve(ctx, *sendingTo, false)
		if err != nil {
			return false, err
		}
		if doid, ok := dst["_id"].(primitive.ObjectID); !ok || doid != newDstOID {
			return false, fail("sending_to is only valid for transfers")
		}
	}
	if newType != "transfer" && hadDst {
		hadDst = false
		changed = true
	}
	if date != nil {
		d := strings.TrimSpace(*date)
		if !dateRE.MatchString(d) {
			return false, fail("date must be YYYY-MM-DD")
		}
		if _, err := time.Parse("2006-01-02", d); err != nil {
			return false, fail("date %q is not a real calendar date — use YYYY-MM-DD", d)
		}
		if d != newDate {
			changed = true
		}
		newDate = d
	}
	if note != nil {
		if n := truncateBytes(*note, 300); n != newNote {
			changed = true
		}
		newNote = truncateBytes(*note, 300)
	}
	set := bson.M{"amount": newPaise, "type": newType, "category": newCat,
		"date": newDate, "note": newNote, "accountId": newAcctOID}
	unset := bson.M{}
	if hadDst {
		set["sending_to"] = newDstOID
	} else {
		unset["sending_to"] = ""
	}
	update := bson.M{"$set": set}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	if _, err := s.txns.UpdateOne(ctx, bson.M{"_id": oid}, update); err != nil {
		return false, err
	}
	return changed, nil
}

// queryFilter builds the normalized Mongo filter Query scans: date range plus optional type/category/account (either side of a
// transfer). Normalization and membership checks live here, once.
func (s *Store) queryFilter(ctx context.Context, start, end string, typ, category, account *string) (bson.M, error) {
	filt := bson.M{"date": bson.M{"$gte": start, "$lte": end}, "deletedAt": bson.M{"$exists": false}}
	if typ != nil {
		t := strings.ToLower(strings.TrimSpace(*typ))
		if !contains(txTypes, t) {
			return nil, fail("type %q does not exist — use one of %v", *typ, txTypes)
		}
		filt["type"] = t
	}
	if category != nil {
		c, err := s.requireCategory(ctx, "query_transactions", *category)
		if err != nil {
			return nil, err
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
// snapshot: a concurrent log between page fetches can shift
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
		r["amount_paise"] = paiseOf(r["amount"])
		delete(r, "amount")
		r["accountId"] = idHex(r["accountId"])
		r["account"] = names[idHex(r["accountId"])]
		if st, ok := r["sending_to"]; ok {
			r["sending_to"] = idHex(st)
			r["sending_to_account"] = names[idHex(st)]
		}
	}
	return rows, total, nil
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
	// paise, not ₹10 — the store and the MCP surface are both paise-native,
	// so the handler forwards amount_paise straight through with no
	// conversion. Rupee floats are still accepted from direct store callers
	// but must be exact paise (round-trip check) or fail — a float used to
	// silently miss post-migration (stored 1010 vs filter 10.1), the
	// silent-zero-match class of bug. Non-numerics fail instead of matching
	// nothing.
	out := bson.M{"deletedAt": bson.M{"$exists": false}}
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
			// Categories are user-managed (money_categories): the live set is
			// checked, never a fixed list.
			if vs, ok := v.(string); ok {
				if k == "type" {
					norm := strings.ToLower(strings.TrimSpace(vs))
					if !contains(txTypes, norm) {
						return nil, fail("type %q does not exist — use one of %v", vs, txTypes)
					}
					out[k] = norm
				} else if c, err := s.requireCategory(ctx, "delete_transactions", vs); err != nil {
					return nil, err
				} else {
					out[k] = c
				}
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
// (the tool rejects 0 as "absent").
func (s *Store) migrateFloatMoney(ctx context.Context) error {
	// Transactions only now: account balances migrate via
	// migrateStoredBalances below, which reads the legacy `balance` with the
	// same paise conversion.
	cur, err := s.txns.Find(ctx, bson.M{"amount": bson.M{"$type": []string{"double", "decimal"}}})
	if err != nil {
		return err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return err
	}
	for _, r := range rows {
		paise := paiseOf(r["amount"])
		if paise < 1 {
			paise = 1
		}
		if _, err := s.txns.UpdateOne(ctx, bson.M{"_id": r["_id"]},
			bson.M{"$set": bson.M{"amount": paise}}); err != nil {
			return err
		}
	}
	return nil
}

// migrateStoredBalances converts legacy stored running totals to opening
// seeds (immutable by ledger writes — only edit_account changes them later),
// once: opening = stored balance − signed leg sum. New code
// never writes `balance`, so each legacy account matches once (the filter
// requires the legacy field and no seed); reruns and fresh databases find
// nothing to do. Runs after the amount migration so legs sum from paise.
func (s *Store) migrateStoredBalances(ctx context.Context) error {
	legs, err := s.derivedLegs(ctx)
	if err != nil {
		return err
	}
	cur, err := s.accts.Find(ctx, bson.M{
		"balance":        bson.M{"$exists": true},
		"openingBalance": bson.M{"$exists": false},
	})
	if err != nil {
		return err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return err
	}
	for _, r := range rows {
		opening := paiseOf(r["balance"]) - legs[idHex(r["_id"])]
		if _, err := s.accts.UpdateOne(ctx, bson.M{"_id": r["_id"]}, bson.M{
			"$set":   bson.M{"openingBalance": opening},
			"$unset": bson.M{"balance": ""},
		}); err != nil {
			return err
		}
	}
	return nil
}

// EnsureSchema creates collections + indexes (idempotent). The legacy
// expiry drop runs first: if it fails transiently, nothing else has run yet,
// so the restart retries the same first step. Then indexes: if the data
// migration below fails transiently, the unique guarantees are already in
// place for the restart. Called by New so fresh databases get the
// unique-name and query indexes immediately.
// Also migrates legacy float rupees to int64 paise (see ToPaise) — new code
// writes longs only, so the migration converges on first boot.
//
// Expiry is gone: rows are permanent unless deleted. A database created by
// an older build still carries the ttl_expiresAt index (which would keep
// deleting) and inert expiresAt fields, so both are removed here,
// idempotently — after the first boot there is nothing left to remove.
func (s *Store) EnsureSchema(ctx context.Context) error {
	if err := s.dropLegacyExpiry(ctx); err != nil {
		return err
	}
	for _, c := range []struct {
		name string
		keys bson.D
		opts *options.IndexOptions
	}{
		{accounts, bson.D{{Key: "name", Value: 1}}, options.Index().SetUnique(true).SetName("uniq_name")},
		{transactions, bson.D{{Key: "idempotencyKey", Value: 1}},
			options.Index().SetName("uniq_idempotency").SetUnique(true).SetSparse(true)},
		{transactions, bson.D{{Key: "accountId", Value: 1}, {Key: "date", Value: 1}}, options.Index().SetName("acct_date")},
		{transactions, bson.D{{Key: "sending_to", Value: 1}, {Key: "date", Value: 1}}, options.Index().SetName("sendingto_date").SetSparse(true)},
		{transactions, bson.D{{Key: "date", Value: 1}, {Key: "type", Value: 1}}, options.Index().SetName("date_type")},
		{transactions, bson.D{{Key: "category", Value: 1}, {Key: "date", Value: 1}}, options.Index().SetName("cat_date")},
		// Soft-delete TTL (#570): flagged rows are removed 10 days after
		// flagging. The TTL clock runs on deletedAt (flag time), never on
		// the transaction date — money is still permanent until deleted.
		{transactions, bson.D{{Key: "deletedAt", Value: 1}},
			options.Index().SetName("ttl_deleted_at").SetExpireAfterSeconds(864000)},
		{budgets, bson.D{{Key: "category", Value: 1}, {Key: "month", Value: 1}},
			options.Index().SetName("uniq_budget_category_month").SetUnique(true)},
	} {
		if _, err := s.db.Collection(c.name).Indexes().CreateOne(ctx, mongoDrv.IndexModel{Keys: c.keys, Options: c.opts}); err != nil {
			return err
		}
	}
	if err := s.migrateFloatMoney(ctx); err != nil {
		return err
	}
	if err := s.migrateStoredBalances(ctx); err != nil {
		return err
	}
	return s.ensureCategories(ctx)
}

// dropLegacyExpiry removes the expiry machinery an older build installed:
// the ttl_expiresAt index (which would otherwise keep deleting rows) and the
// now-unread expiresAt fields. Plain ops, never in a transaction — index
// drops are not allowed inside one. Both steps match nothing on a fresh or
// already-migrated database, so reruns are cheap no-ops.
func (s *Store) dropLegacyExpiry(ctx context.Context) error {
	cur, err := s.txns.Indexes().List(ctx)
	if err != nil {
		return err
	}
	for cur.Next(ctx) {
		var spec bson.M
		if err := cur.Decode(&spec); err != nil {
			return err
		}
		if name, _ := spec["name"].(string); name == "ttl_expiresAt" {
			if _, err := s.txns.Indexes().DropOne(ctx, name); err != nil {
				return err
			}
			break
		}
	}
	if err := cur.Err(); err != nil {
		return err
	}
	_, err = s.txns.UpdateMany(ctx,
		bson.M{"expiresAt": bson.M{"$exists": true}},
		bson.M{"$unset": bson.M{"expiresAt": ""}})
	return err
}
