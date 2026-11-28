package money

import (
	"context"
	"math"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const budgets = "money_budgets"

// Budget is one dated limit row: category + month is the identity, and the
// limit applies from that month until a newer row supersedes it. Limits are
// int64 paise — the tool surface speaks paise, matching storage exactly.
type Budget struct {
	ID                string
	Category          string
	Month             string
	MonthlyLimitPaise int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// CheckBudgetMonth validates a YYYY-MM month: four-digit year, real month
// 01-12. It trims but never defaults — an absent month is a missing month.
func CheckBudgetMonth(tool, v string) (string, error) {
	m := strings.TrimSpace(v)
	if m == "" {
		return "", fail("%s month is required — use YYYY-MM", tool)
	}
	if len(m) != 7 || m[4] != '-' {
		return "", fail("%s month must be YYYY-MM — got %q", tool, v)
	}
	if _, err := time.Parse("2006-01", m); err != nil {
		return "", fail("%s month must be a real YYYY-MM month — got %q", tool, v)
	}
	y, mo, _ := strings.Cut(m, "-")
	if len(y) != 4 || len(mo) != 2 {
		return "", fail("%s month must be YYYY-MM — got %q", tool, v)
	}
	return m, nil
}

// CheckBudgetLimit validates a monthly limit in paise: an integer > 0.
func CheckBudgetLimit(tool string, paise int64) (int64, error) {
	if paise <= 0 {
		return 0, fail("%s monthly_limit_paise must be > 0 — got %d", tool, paise)
	}
	return paise, nil
}

func budgetDocToBudget(r bson.M) Budget {
	b := Budget{}
	if id, ok := r["_id"].(primitive.ObjectID); ok {
		b.ID = id.Hex()
	} else if s, ok := r["_id"].(string); ok {
		b.ID = s
	}
	if c, ok := r["category"].(string); ok {
		b.Category = c
	}
	if m, ok := r["month"].(string); ok {
		b.Month = m
	}
	b.MonthlyLimitPaise = paiseOf(r["monthlyLimit"])
	if t, ok := r["createdAt"].(primitive.DateTime); ok {
		b.CreatedAt = t.Time()
	}
	if t, ok := r["updatedAt"].(primitive.DateTime); ok {
		b.UpdatedAt = t.Time()
	}
	return b
}

func budgetIDHex(v string) (primitive.ObjectID, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(v))
	if err != nil {
		return primitive.ObjectID{}, fail("'%s' is not a budget id", v)
	}
	return oid, nil
}

// CreateBudget stores a dated limit. A duplicate category+month is rejected
// naming the existing row — limits move with update_budget, never overwrite.
func (s *Store) CreateBudget(ctx context.Context, category, month string, monthlyLimitPaise int64) (*Budget, error) {
	cat, err := s.CheckBudgetCategory(ctx, "create_budget", category)
	if err != nil {
		return nil, err
	}
	mo, err := CheckBudgetMonth("create_budget", month)
	if err != nil {
		return nil, err
	}
	paise, err := CheckBudgetLimit("create_budget", monthlyLimitPaise)
	if err != nil {
		return nil, err
	}
	var existing bson.M
	if err := s.budgets.FindOne(ctx, bson.M{"category": cat, "month": mo}).Decode(&existing); err == nil {
		return nil, fail("create_budget %s already has a budget for %s (id %s) — use update_budget to change it",
			cat, mo, oidHex(existing["_id"]))
	}
	now := primitive.NewDateTimeFromTime(time.Now().UTC())
	res, err := s.budgets.InsertOne(ctx, bson.M{
		"category": cat, "month": mo, "monthlyLimit": paise,
		"createdAt": now, "updatedAt": now,
	})
	if err != nil {
		return nil, err
	}
	var stored bson.M
	if err := s.budgets.FindOne(ctx, bson.M{"_id": res.InsertedID}).Decode(&stored); err != nil {
		return nil, err
	}
	b := budgetDocToBudget(stored)
	return &b, nil
}

// ListBudgetsResult is one budget table. At most one row per category carries
// effective_now for the current IST month (the greatest month <= today);
// a category whose only row is in the future has none.
type ListedBudget struct {
	Budget
	EffectiveNow bool
}

// ListBudgets lists every row (or the filtered slice), ordered by category
// ascending then month descending. Filters are optional and open-ended.
func (s *Store) ListBudgets(ctx context.Context, monthFrom, monthTo, category string) ([]ListedBudget, error) {
	filt := bson.M{}
	if category != "" {
		cat, err := s.CheckBudgetCategory(ctx, "list_budgets", category)
		if err != nil {
			return nil, err
		}
		filt["category"] = cat
	}
	if monthFrom != "" {
		if _, err := CheckBudgetMonth("list_budgets", monthFrom); err != nil {
			return nil, err
		}
		filt["month"] = bson.M{"$gte": monthFrom}
	}
	if monthTo != "" {
		if _, err := CheckBudgetMonth("list_budgets", monthTo); err != nil {
			return nil, err
		}
		m, ok := filt["month"].(bson.M)
		if !ok {
			m = bson.M{}
		}
		m["$lte"] = monthTo
		filt["month"] = m
	}
	if monthFrom != "" && monthTo != "" && monthFrom > monthTo {
		return nil, fail("list_budgets month_from %s is after month_to %s", monthFrom, monthTo)
	}
	cur, err := s.budgets.Find(ctx, filt, options.Find().SetSort(bson.D{{Key: "category", Value: 1}, {Key: "month", Value: -1}}))
	if err != nil {
		return nil, err
	}
	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	// effective_now is the row in force for the current IST month, computed
	// over ALL rows for the returned categories — not over the filtered
	// slice. Flagging from the filtered rows marks a superseded row when the
	// filter cuts off the months after it (e.g. month_to before today).
	todayMonth := TodayIST()[:7]
	best := map[string]string{}
	if len(docs) > 0 {
		cats := map[string]bool{}
		for _, d := range docs {
			if c, _ := d["category"].(string); c != "" {
				cats[c] = true
			}
		}
		catFilt := bson.M{}
		if len(cats) == 1 {
			for c := range cats {
				catFilt["category"] = c
			}
		} else if len(cats) > 1 {
			names := make([]string, 0, len(cats))
			for c := range cats {
				names = append(names, c)
			}
			catFilt["category"] = bson.M{"$in": names}
		}
		allCur, err := s.budgets.Find(ctx, catFilt)
		if err != nil {
			return nil, err
		}
		var allDocs []bson.M
		if err := allCur.All(ctx, &allDocs); err != nil {
			return nil, err
		}
		for _, d := range allDocs {
			c, _ := d["category"].(string)
			m, _ := d["month"].(string)
			if m <= todayMonth && m > best[c] {
				best[c] = m
			}
		}
	}
	out := make([]ListedBudget, 0, len(docs))
	for _, d := range docs {
		b := budgetDocToBudget(d)
		out = append(out, ListedBudget{Budget: b, EffectiveNow: b.Month == best[b.Category] && best[b.Category] != ""})
	}
	return out, nil
}

// UpdateBudget changes one row's limit in place. Category and month are
// identity — a typo there is fixed by delete + create, not by update.
func (s *Store) UpdateBudget(ctx context.Context, id string, monthlyLimitPaise int64) (*Budget, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fail("update_budget id is required")
	}
	oid, err := budgetIDHex(id)
	if err != nil {
		return nil, fail("update_budget %v", err)
	}
	paise, err := CheckBudgetLimit("update_budget", monthlyLimitPaise)
	if err != nil {
		return nil, err
	}
	var existing bson.M
	if err := s.budgets.FindOne(ctx, bson.M{"_id": oid}).Decode(&existing); err != nil {
		return nil, fail("update_budget unknown budget id '%s'", strings.TrimSpace(id))
	}
	if paiseOf(existing["monthlyLimit"]) == paise {
		return nil, fail("update_budget monthly_limit is unchanged")
	}
	now := primitive.NewDateTimeFromTime(time.Now().UTC())
	if _, err := s.budgets.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{
		"$set": bson.M{"monthlyLimit": paise, "updatedAt": now},
	}); err != nil {
		return nil, err
	}
	var stored bson.M
	if err := s.budgets.FindOne(ctx, bson.M{"_id": oid}).Decode(&stored); err != nil {
		return nil, err
	}
	b := budgetDocToBudget(stored)
	return &b, nil
}

// DeleteBudgetResult is one deleted (or would-be deleted) version plus what
// would govern its own month afterwards — null/Unbudgeted when that month
// would have no budget. Later surviving versions are unaffected.
type DeleteBudgetResult struct {
	Row              Budget
	DryRun           bool
	Deleted          int
	FallsBackToMonth string
	FallsBackToLimit int64
	HasFallback      bool
	Unbudgeted       bool
}

// DeleteBudget removes one budget version. Dry run (the default) reports the
// row, what would govern its month, and deletes nothing.
func (s *Store) DeleteBudget(ctx context.Context, id string, dryRun bool) (*DeleteBudgetResult, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fail("delete_budget id is required")
	}
	oid, err := budgetIDHex(id)
	if err != nil {
		return nil, fail("delete_budget %v", err)
	}
	var existing bson.M
	if err := s.budgets.FindOne(ctx, bson.M{"_id": oid}).Decode(&existing); err != nil {
		return nil, fail("delete_budget unknown budget id '%s'", strings.TrimSpace(id))
	}
	row := budgetDocToBudget(existing)
	res := &DeleteBudgetResult{Row: row, DryRun: dryRun}
	// Fallback: what would govern the deleted row's own month afterwards —
	// the greatest earlier month for the same category. Unbudgeted therefore
	// means "that month would have no budget", never "the whole category is
	// budgetless": later surviving versions still govern their own months.
	cur, err := s.budgets.Find(ctx,
		bson.M{"category": row.Category, "month": bson.M{"$lt": row.Month}},
		options.Find().SetSort(bson.D{{Key: "month", Value: -1}}).SetLimit(1))
	if err != nil {
		return nil, err
	}
	var earlier []bson.M
	if err := cur.All(ctx, &earlier); err != nil {
		return nil, err
	}
	if len(earlier) == 0 {
		res.Unbudgeted = true
	} else {
		fb := budgetDocToBudget(earlier[0])
		res.HasFallback = true
		res.FallsBackToMonth = fb.Month
		res.FallsBackToLimit = fb.MonthlyLimitPaise
	}
	if dryRun {
		return res, nil
	}
	del, err := s.budgets.DeleteOne(ctx, bson.M{"_id": oid})
	if err != nil {
		return nil, err
	}
	res.Deleted = int(del.DeletedCount)
	return res, nil
}

// resolveLimit returns the effective monthlyLimit paise for category in
// month m: the row with the greatest month <= m. ok is false when the
// category is unbudgeted for m — not an error.
func (s *Store) resolveLimit(ctx context.Context, category, m string) (limit int64, ok bool, err error) {
	cur, err := s.budgets.Find(ctx,
		bson.M{"category": category, "month": bson.M{"$lte": m}},
		options.Find().SetSort(bson.D{{Key: "month", Value: -1}}).SetLimit(1))
	if err != nil {
		return 0, false, err
	}
	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		return 0, false, err
	}
	if len(docs) == 0 {
		return 0, false, nil
	}
	return paiseOf(docs[0]["monthlyLimit"]), true, nil
}

// budgetedCategories returns every category with at least one budget row.
func (s *Store) budgetedCategories(ctx context.Context) ([]string, error) {
	vals, err := s.budgets.Distinct(ctx, "category", bson.M{})
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, v := range vals {
		if c, ok := v.(string); ok {
			out = append(out, c)
		}
	}
	sort.Strings(out)
	return out, nil
}

// BudgetStatusRow is one category's budget status. Projected fields are null
// unless the window overlaps the current IST month.
type BudgetStatusRow struct {
	Category            string
	Budgeted            bool
	LimitPaise          int64
	SpendPaise          int64
	RemainingPaise      *int64
	UsedPct             *float64
	OverBudget          bool
	ProjectedMonthPaise *int64
	ProjectedOverBudget *bool
}

// BudgetStatusResult is one budget table plus its flat footer. Totals count
// budgeted categories only; unbudgeted spend is reported separately.
type BudgetStatusResult struct {
	Rows                      []BudgetStatusRow
	TotalLimitPaise           int64
	TotalSpendPaise           int64
	TotalRemainingPaise       int64
	TotalUsedPct              *float64
	TotalOverBudget           bool
	BudgetedCategories        int
	TotalUnbudgetedSpendPaise int64
	ProjectionBasis           string
	CategoriesApplied         []string
	SortBy                    string
	Order                     string
}

var budgetStatusSorts = []string{"remaining", "used", "spend", "limit", "name"}

// monthsTouched lists every YYYY-MM the window [start, end] touches.
func monthsTouched(start, end string) ([]string, error) {
	ts, err := parseDay(start)
	if err != nil {
		return nil, err
	}
	te, err := parseDay(end)
	if err != nil {
		return nil, err
	}
	out := []string{}
	cur := time.Date(ts.Year(), ts.Month(), 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(te.Year(), te.Month(), 1, 0, 0, 0, 0, time.UTC)
	for !cur.After(last) {
		out = append(out, cur.Format("2006-01"))
		cur = cur.AddDate(0, 1, 0)
	}
	return out, nil
}

// BudgetStatus compares spend vs prorated limit for the window. The limit
// for each category is the sum over touched months of the effective monthly
// limit prorated per covered day. Unbudgeted categories share the table with
// budgeted:false. Categories[] picks rows; the footer follows the selection.
func (s *Store) BudgetStatus(ctx context.Context, start, end string, categories []string, sortBy, order string) (*BudgetStatusResult, error) {
	months, err := monthsTouched(start, end)
	if err != nil {
		return nil, err
	}
	// Spend per category: expense rows in the window.
	spendRows, err := s.scanWindow(ctx, start, end, []string{"expense"}, nil, nil)
	if err != nil {
		return nil, err
	}
	spend := map[string]int64{}
	for _, r := range spendRows {
		spend[r.Category] += r.AmountPaise
	}
	budgeted, err := s.budgetedCategories(ctx)
	if err != nil {
		return nil, err
	}
	inSet := map[string]bool{}
	for _, c := range categories {
		inSet[c] = true
	}
	pick := func(c string) bool {
		return len(categories) == 0 || inSet[c]
	}
	// Union of budgeted categories and spent categories, honoring the picker.
	union := map[string]bool{}
	for _, c := range budgeted {
		if pick(c) {
			union[c] = true
		}
	}
	for c := range spend {
		if c == "salary" {
			continue
		}
		if pick(c) {
			union[c] = true
		}
	}
	// Projection basis: does the window overlap the current IST month?
	today := TodayIST()
	curMonth := today[:7]
	overlapsNow := false
	for _, m := range months {
		if m == curMonth {
			overlapsNow = true
		}
	}
	var monthSpend map[string]int64
	if overlapsNow {
		first := curMonth + "-01"
		t, _ := time.Parse("2006-01", curMonth)
		last := time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
		mrows, err := s.scanWindow(ctx, first, last, []string{"expense"}, nil, nil)
		if err != nil {
			return nil, err
		}
		monthSpend = map[string]int64{}
		for _, r := range mrows {
			monthSpend[r.Category] += r.AmountPaise
		}
	}
	elapsed := 0
	dim := 0
	if overlapsNow {
		t, _ := parseDay(today)
		elapsed = t.Day()
		dim = daysInMonth(t.Year(), t.Month())
	}
	out := &BudgetStatusResult{
		Rows:              []BudgetStatusRow{},
		ProjectionBasis:   "calendar_month",
		CategoriesApplied: append([]string{}, categories...),
		SortBy:            sortBy,
		Order:             order,
	}
	for cat := range union {
		row := BudgetStatusRow{Category: cat}
		_, ok, err := s.resolveLimit(ctx, cat, months[len(months)-1])
		if err != nil {
			return nil, err
		}
		if ok {
			// Prorate over every touched month.
			var total int64
			for _, m := range months {
				ml, okm, err := s.resolveLimit(ctx, cat, m)
				if err != nil {
					return nil, err
				}
				if !okm {
					continue
				}
				t, _ := time.Parse("2006-01", m)
				d := daysInMonth(t.Year(), t.Month())
				first := m + "-01"
				last := time.Date(t.Year(), t.Month()+1, 0, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
				lo := first
				if lo < start {
					lo = start
				}
				hi := last
				if hi > end {
					hi = end
				}
				n, err := DaysBetween(lo, hi)
				if err != nil {
					return nil, err
				}
				total += prorate(ml, n, d)
			}
			row.Budgeted = true
			row.LimitPaise = total
		}
		row.SpendPaise = spend[cat]
		if row.Budgeted {
			rem := row.LimitPaise - row.SpendPaise
			row.RemainingPaise = &rem
			used := roundPct2(row.SpendPaise, row.LimitPaise)
			if row.LimitPaise == 0 {
				used = 0
			}
			row.UsedPct = &used
			row.OverBudget = row.SpendPaise > row.LimitPaise
			out.TotalLimitPaise += row.LimitPaise
			out.TotalSpendPaise += row.SpendPaise
			out.BudgetedCategories++
		} else {
			out.TotalUnbudgetedSpendPaise += row.SpendPaise
		}
		if overlapsNow && elapsed > 0 {
			ms := monthSpend[cat]
			proj := (2*ms*int64(dim) + int64(elapsed)) / (2 * int64(elapsed))
			row.ProjectedMonthPaise = &proj
			if row.Budgeted {
				ml, okm, err := s.resolveLimit(ctx, cat, curMonth)
				if err != nil {
					return nil, err
				}
				if okm {
					over := proj > ml
					row.ProjectedOverBudget = &over
				}
			}
		}
		out.Rows = append(out.Rows, row)
	}
	out.TotalRemainingPaise = out.TotalLimitPaise - out.TotalSpendPaise
	if out.TotalLimitPaise > 0 {
		u := roundPct2(out.TotalSpendPaise, out.TotalLimitPaise)
		out.TotalUsedPct = &u
	}
	out.TotalOverBudget = out.BudgetedCategories > 0 && out.TotalSpendPaise > out.TotalLimitPaise
	sort.SliceStable(out.Rows, func(i, j int) bool {
		a, b := out.Rows[i], out.Rows[j]
		var less bool
		switch sortBy {
		case "name":
			less = a.Category < b.Category
		case "used":
			av, bv := pctOrNeg(a.UsedPct), pctOrNeg(b.UsedPct)
			if av != bv {
				less = av < bv
			} else {
				less = a.Category < b.Category
			}
		case "spend":
			if a.SpendPaise != b.SpendPaise {
				less = a.SpendPaise < b.SpendPaise
			} else {
				less = a.Category < b.Category
			}
		case "limit":
			if a.LimitPaise != b.LimitPaise {
				less = a.LimitPaise < b.LimitPaise
			} else {
				less = a.Category < b.Category
			}
		default: // remaining: smallest first (most over budget first); nulls last
			av, bv := remOrMax(a.RemainingPaise), remOrMax(b.RemainingPaise)
			if av != bv {
				less = av < bv
			} else {
				less = a.Category < b.Category
			}
		}
		if order == "asc" {
			return less
		}
		// desc: flip value compares, keep name tie-breaks ascending.
		switch sortBy {
		case "name":
			return !less && a.Category != b.Category
		case "used":
			av, bv := pctOrNeg(a.UsedPct), pctOrNeg(b.UsedPct)
			if av != bv {
				return av > bv
			}
			return a.Category < b.Category
		case "spend":
			if a.SpendPaise != b.SpendPaise {
				return a.SpendPaise > b.SpendPaise
			}
			return a.Category < b.Category
		case "limit":
			if a.LimitPaise != b.LimitPaise {
				return a.LimitPaise > b.LimitPaise
			}
			return a.Category < b.Category
		default:
			av, bv := remOrMax(a.RemainingPaise), remOrMax(b.RemainingPaise)
			// Nulls (unbudgeted) stay last in both directions.
			if (a.RemainingPaise == nil) != (b.RemainingPaise == nil) {
				return b.RemainingPaise == nil
			}
			if av != bv {
				return av > bv
			}
			return a.Category < b.Category
		}
	})
	return out, nil
}

func pctOrNeg(p *float64) float64 {
	if p == nil {
		return -1
	}
	return *p
}

func remOrMax(p *int64) int64 {
	if p == nil {
		return math.MaxInt64
	}
	return *p
}
