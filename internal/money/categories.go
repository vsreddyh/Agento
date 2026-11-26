package money

import (
	"context"
	"strings"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const categories = "money_categories"

// maxCategoryBytes caps a category name in bytes (what Mongo measures), so
// the limit is one no caller can mistake for a character count.
const maxCategoryBytes = 64

// normalizeCategoryName trims and lowercases a category. It never validates:
// blank means "absent" at some call sites and "other" at others, so the
// caller decides what blank means after normalizing.
func normalizeCategoryName(v string) string {
	return strings.ToLower(strings.TrimSpace(v))
}

// ensureCategories creates the unique name index and seeds the default set
// on an empty collection. Reruns and fresh-code paths find rows and do
// nothing, so seeding is idempotent by construction.
func (s *Store) ensureCategories(ctx context.Context) error {
	if _, err := s.cats.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "name", Value: 1}},
		Options: options.Index().SetUnique(true).SetName("uniq_name"),
	}); err != nil {
		return err
	}
	n, err := s.cats.CountDocuments(ctx, bson.M{})
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	docs := make([]any, 0, len(DefaultCategories))
	for _, c := range DefaultCategories {
		docs = append(docs, bson.M{"name": c})
	}
	_, err = s.cats.InsertMany(ctx, docs)
	return err
}

// categoryList returns every category name, sorted. It falls back to the
// defaults only when the collection is empty (a path that bypassed seeding),
// so reads never fail on a missing set.
func (s *Store) categoryList(ctx context.Context) ([]string, error) {
	cur, err := s.cats.Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(docs))
	for _, d := range docs {
		if c, ok := d["name"].(string); ok && c != "" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		out = append([]string{}, DefaultCategories...)
	}
	return out, nil
}

// requireCategory normalizes v and rejects blank or unknown names. The error
// lists the live set, never a hard-coded one.
func (s *Store) requireCategory(ctx context.Context, tool, v string) (string, error) {
	c := normalizeCategoryName(v)
	if c == "" {
		return "", fail("%s category is required", tool)
	}
	live, err := s.categoryList(ctx)
	if err != nil {
		return "", err
	}
	for _, e := range live {
		if e == c {
			return c, nil
		}
	}
	return "", fail("%s category %q does not exist — use one of %v", tool, v, live)
}

// NormalizeCategory trims and lowercases one optional category. Blank means
// absent (""). An unknown name is rejected, like writes — never zero-filled.
func (s *Store) NormalizeCategory(ctx context.Context, tool, v string) (string, error) {
	if normalizeCategoryName(v) == "" {
		return "", nil
	}
	return s.requireCategory(ctx, tool, v)
}

// NormalizeCategories trims, lowercases, drops blanks and dedupes a plural
// filter, preserving first-seen order. Unknown names reject the whole call.
func (s *Store) NormalizeCategories(ctx context.Context, tool string, in []string) ([]string, error) {
	out := []string{}
	seen := map[string]bool{}
	for _, v := range in {
		c := normalizeCategoryName(v)
		if c == "" {
			continue
		}
		if _, err := s.requireCategory(ctx, tool, v); err != nil {
			return nil, err
		}
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out, nil
}

// CheckBudgetCategory normalizes a budget category: trimmed, lowercased,
// must exist — and salary is income, so it cannot be budgeted.
func (s *Store) CheckBudgetCategory(ctx context.Context, tool, v string) (string, error) {
	c, err := s.requireCategory(ctx, tool, v)
	if err != nil {
		return "", err
	}
	if c == "salary" {
		return "", fail("%s salary is income and cannot be budgeted", tool)
	}
	return c, nil
}

// ListCategories returns the user-managed set, sorted, with 'other' always
// present as the blank fallback.
func (s *Store) ListCategories(ctx context.Context) ([]string, error) {
	return s.categoryList(ctx)
}

// checkCategoryName validates a name for create/rename: non-blank, within
// the byte cap, normalized. Existence is the caller's check.
func checkCategoryName(tool, v string) (string, error) {
	c := normalizeCategoryName(v)
	if c == "" {
		return "", fail("%s category name is required", tool)
	}
	if len(c) > maxCategoryBytes {
		return "", fail("%s category name is %d bytes; the limit is %d", tool, len(c), maxCategoryBytes)
	}
	return c, nil
}

// CreateCategory stores a new category. Names are unique (index-backed),
// trimmed and lowercased. 'other' is reserved and already seeded, so creating
// it reports the duplicate like any taken name.
func (s *Store) CreateCategory(ctx context.Context, name string) (string, error) {
	c, err := checkCategoryName("create_category", name)
	if err != nil {
		return "", err
	}
	if _, err := s.cats.InsertOne(ctx, bson.M{"name": c}); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return "", fail("category '%s' already exists", c)
		}
		return "", err
	}
	return c, nil
}

// UpdateCategory renames a category everywhere: the registry row plus every
// transaction and budget row carrying the old name. 'other' cannot be renamed
// (it is the blank fallback), and nothing can be renamed to 'other'.
// Renaming to the same name is a no-op rejection, not a silent success.
func (s *Store) UpdateCategory(ctx context.Context, old, new string) (string, int64, int64, error) {
	from := normalizeCategoryName(old)
	if from == "" {
		return "", 0, 0, fail("update_category category is required")
	}
	to, err := checkCategoryName("update_category", new)
	if err != nil {
		return "", 0, 0, err
	}
	if from == "other" {
		return "", 0, 0, fail("update_category 'other' is the blank fallback and cannot be renamed")
	}
	if to == "other" {
		return "", 0, 0, fail("update_category cannot rename to 'other' — it is the reserved blank fallback")
	}
	if from == to {
		return "", 0, 0, fail("update_category category is unchanged")
	}
	live, err := s.categoryList(ctx)
	if err != nil {
		return "", 0, 0, err
	}
	found, taken := false, false
	for _, e := range live {
		if e == from {
			found = true
		}
		if e == to {
			taken = true
		}
	}
	if !found {
		return "", 0, 0, fail("update_category unknown category '%s'", strings.TrimSpace(old))
	}
	if taken {
		return "", 0, 0, fail("update_category category '%s' already exists", to)
	}
	if _, err := s.cats.UpdateOne(ctx, bson.M{"name": from}, bson.M{"$set": bson.M{"name": to}}); err != nil {
		return "", 0, 0, err
	}
	txnRes, err := s.txns.UpdateMany(ctx, bson.M{"category": from}, bson.M{"$set": bson.M{"category": to}})
	if err != nil {
		return "", 0, 0, err
	}
	budRes, err := s.budgets.UpdateMany(ctx, bson.M{"category": from}, bson.M{"$set": bson.M{"category": to}})
	if err != nil {
		return "", 0, 0, err
	}
	return to, txnRes.ModifiedCount, budRes.ModifiedCount, nil
}

// categoryRefCounts reports how many transaction and budget rows carry each
// of the named categories.
func (s *Store) categoryRefCounts(ctx context.Context, names []string) (map[string][2]int64, error) {
	out := make(map[string][2]int64, len(names))
	// Live rows only: a row awaiting TTL removal no longer counts as using
	// the category, so a soft-deleted reference never blocks a delete guard.
	live := liveFilter()
	for _, n := range names {
		var txN, budN int64
		var err error
		if txN, err = s.txns.CountDocuments(ctx, bson.M{"category": n, "deletedAt": live["deletedAt"]}); err != nil {
			return nil, err
		}
		if budN, err = s.budgets.CountDocuments(ctx, bson.M{"category": n}); err != nil {
			return nil, err
		}
		out[n] = [2]int64{txN, budN}
	}
	return out, nil
}

// DeleteCategory removes an unused category. Dry run (the default) reports
// reference counts and deletes nothing. With dry_run=false the delete is
// rejected while any transaction or budget row references the category,
// unless reassign_to names another existing category to move those rows to.
// 'other' cannot be deleted — it is the blank fallback.
func (s *Store) DeleteCategory(ctx context.Context, name, reassignTo string, dryRun bool) (string, int64, int64, error) {
	c := normalizeCategoryName(name)
	if c == "" {
		return "", 0, 0, fail("delete_category category is required")
	}
	if c == "other" {
		return "", 0, 0, fail("delete_category 'other' is the blank fallback and cannot be deleted")
	}
	live, err := s.categoryList(ctx)
	if err != nil {
		return "", 0, 0, err
	}
	exists := false
	for _, e := range live {
		if e == c {
			exists = true
			break
		}
	}
	if !exists {
		return "", 0, 0, fail("delete_category unknown category '%s'", strings.TrimSpace(name))
	}
	counts, err := s.categoryRefCounts(ctx, []string{c})
	if err != nil {
		return "", 0, 0, err
	}
	txN, budN := counts[c][0], counts[c][1]
	// Validate the target whenever it would actually be used — including in
	// dry-run mode, so the preview catches the typo instead of the live
	// call. Skipped when nothing references the category (reassign_to is
	// ignored there, even a typo'd one).
	to := normalizeCategoryName(reassignTo)
	if to != "" && txN+budN > 0 {
		if to == c {
			return "", 0, 0, fail("delete_category reassign_to must differ from the deleted category")
		}
		if _, err := s.requireCategory(ctx, "delete_category", to); err != nil {
			return "", 0, 0, err
		}
	}
	if dryRun {
		return c, txN, budN, nil
	}
	// Nothing references it: reassign_to is irrelevant (even a typo'd one),
	// so skip validation and delete outright.
	if txN+budN == 0 {
		if _, err := s.cats.DeleteOne(ctx, bson.M{"name": c}); err != nil {
			return "", 0, 0, err
		}
		return c, 0, 0, nil
	}
	if to == "" {
		return "", 0, 0, fail("delete_category '%s' is used by %d transactions and %d budgets — pass reassign_to to move them first", c, txN, budN)
	}
	txnRes, err := s.txns.UpdateMany(ctx, bson.M{"category": c}, bson.M{"$set": bson.M{"category": to}})
	if err != nil {
		return "", 0, 0, err
	}
	budRes, err := s.budgets.UpdateMany(ctx, bson.M{"category": c}, bson.M{"$set": bson.M{"category": to}})
	if err != nil {
		return "", 0, 0, err
	}
	if _, err := s.cats.DeleteOne(ctx, bson.M{"name": c}); err != nil {
		return "", 0, 0, err
	}
	// Echo the moved counts, not zeros — the operator should see that rows
	// moved, matching update_category's transactions_renamed/budgets_renamed.
	return c, txnRes.ModifiedCount, budRes.ModifiedCount, nil
}
