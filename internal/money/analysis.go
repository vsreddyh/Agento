// Package money analysis: read-only aggregations over the ledger.
//
// Every analysis here recomputes from the ledger on each call — no stored
// aggregates, no cache — so edits and deletes are visible on the next call.
// Money amounts aggregate as int64 paise (never floats); percentages are
// display values rounded to 2 decimals, half-up. Dates are stored as
// YYYY-MM-DD strings, so bucketing, zero-fill and proration happen in Go,
// where the rules are exact and unit-testable without a database.
//
// The store methods in this file scan (MongoDB match) and then compute with
// the pure helpers below the scan section, so the math stays testable on
// synthetic rows.
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

// TxRow is one ledger row in analysis-native form: paise-exact, with both
// account legs resolved to names. Transfers carry both legs; every other row
// leaves the sending-to fields empty.
type TxRow struct {
	ID               string
	Date             string
	Type             string
	Category         string
	Note             string
	AmountPaise      int64
	AccountID        string
	Account          string
	SendingToID      string
	SendingToAccount string
}

// resolveAccountIDs maps name-or-hex values to ObjectIDs. An unknown name
// resolves to the nil id — no stored row carries it, so an $in filter over it
// matches nothing (#619) instead of failing the whole analysis, and instead of
// widening to "all accounts" when every requested name was unknown.
func (s *Store) resolveAccountIDs(ctx context.Context, names []string) ([]primitive.ObjectID, error) {
	out := make([]primitive.ObjectID, 0, len(names))
	for _, n := range names {
		acct, found, err := s.lookupAccount(ctx, n)
		if err != nil {
			return nil, err
		}
		if !found {
			out = append(out, primitive.NilObjectID)
			continue
		}
		oid, ok := acct["_id"].(primitive.ObjectID)
		if !ok {
			return nil, fail("account '%s' has no id", n)
		}
		out = append(out, oid)
	}
	return out, nil
}

// scanWindow fetches every row in [start, end] matching the optional filters.
// Empty types/categories/accountIDs mean "all". Types and categories must
// already be normalized (lowercase, trimmed). `type` is a closed set, so an
// unknown value fails; category and account are dynamic refs, so a name that
// does not exist simply matches no row (#619).
func (s *Store) scanWindow(ctx context.Context, start, end string, types, categories []string, accountIDs []primitive.ObjectID) ([]TxRow, error) {
	filt := bson.M{"date": bson.M{"$gte": start, "$lte": end}, "deletedAt": bson.M{"$exists": false}}
	if len(types) > 0 {
		for _, t := range types {
			if !contains(txTypes, t) {
				return nil, fail("type %q does not exist — use one of %v", t, txTypes)
			}
		}
		filt["type"] = bson.M{"$in": types}
	}
	if len(categories) > 0 {
		filt["category"] = bson.M{"$in": categories}
	}
	if len(accountIDs) > 0 {
		filt["$or"] = []bson.M{{"accountId": bson.M{"$in": accountIDs}}, {"sending_to": bson.M{"$in": accountIDs}}}
	}
	cur, err := s.txns.Find(ctx, filt, options.Find().SetSort(bson.D{{Key: "date", Value: 1}, {Key: "_id", Value: 1}}))
	if err != nil {
		return nil, err
	}
	var docs []bson.M
	if err := cur.All(ctx, &docs); err != nil {
		return nil, err
	}
	names, err := s.accountNames(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]TxRow, 0, len(docs))
	for _, r := range docs {
		row := TxRow{
			ID:          oidHex(r["_id"]),
			AmountPaise: paiseOf(r["amount"]),
			AccountID:   idHex(r["accountId"]),
		}
		if d, _ := r["date"].(string); d != "" {
			row.Date = d
		}
		if t, _ := r["type"].(string); t != "" {
			row.Type = t
		}
		if c, _ := r["category"].(string); c != "" {
			row.Category = c
		}
		if n, _ := r["note"].(string); n != "" {
			row.Note = n
		}
		row.Account = names[row.AccountID]
		if st, ok := r["sending_to"]; ok {
			row.SendingToID = idHex(st)
			row.SendingToAccount = names[row.SendingToID]
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// --- Input normalizers (handlers call these before any store touch) ---

// NormalizeSide validates the expense|income discriminator.
func NormalizeSide(tool, v string) (string, error) {
	sd := strings.ToLower(strings.TrimSpace(v))
	if sd == "" {
		return "", fail("%s side is required: expense or income", tool)
	}
	if sd != "expense" && sd != "income" {
		return "", fail("%s side must be expense or income — got %q", tool, v)
	}
	return sd, nil
}

// NormalizeBucket validates the day|week|month discriminator.
func NormalizeBucket(tool, v string) (string, error) {
	b := strings.ToLower(strings.TrimSpace(v))
	if b == "" {
		return "", fail("%s bucket is required: day, week or month", tool)
	}
	if b != "day" && b != "week" && b != "month" {
		return "", fail("%s bucket must be day, week or month — got %q", tool, v)
	}
	return b, nil
}

// NormalizeOrder validates asc|desc, defaulting to def on blank.
func NormalizeOrder(tool, v, def string) (string, error) {
	o := strings.ToLower(strings.TrimSpace(v))
	if o == "" {
		return def, nil
	}
	if o != "asc" && o != "desc" {
		return "", fail("%s order must be asc or desc — got %q", tool, v)
	}
	return o, nil
}

// NormalizeSort validates a caller-chosen sort key against the tool's enum,
// defaulting to def on blank. The default is the tool's canonical order.
func NormalizeSort(tool, v, def string, allowed []string) (string, error) {
	sb := strings.ToLower(strings.TrimSpace(v))
	if sb == "" {
		return def, nil
	}
	if !contains(allowed, sb) {
		return "", fail("%s sort_by must be %s — got %q", tool, strings.Join(allowed, ", "), v)
	}
	return sb, nil
}

// SensitivityRatio maps a sensitivity name to its outlier ratio.
func SensitivityRatio(name string) float64 {
	switch name {
	case "low":
		return 2.00
	case "high":
		return 1.25
	default:
		return 1.50
	}
}

// NormalizeSensitivity validates low|medium|high, defaulting to medium.
func NormalizeSensitivity(tool, v string) (string, error) {
	sn := strings.ToLower(strings.TrimSpace(v))
	if sn == "" {
		return "medium", nil
	}
	if sn != "low" && sn != "medium" && sn != "high" {
		return "", fail("%s sensitivity must be low, medium or high — got %q", tool, v)
	}
	return sn, nil
}

// --- Math helpers (pure; paise stay int64 throughout) ---

// avgPaise divides total by n, rounding half-up to the nearest paise.
// Totals and counts here are non-negative, so the integer form holds.
func avgPaise(total int64, n int) int64 {
	if n <= 0 {
		return 0
	}
	return (2*total + int64(n)) / (2 * int64(n))
}

// roundPct2 renders num/den as a percentage rounded half-up to 2 decimals.
// Callers handle den == 0 (null, not zero) before calling.
func roundPct2(num, den int64) float64 {
	return math.Round(float64(num)*10000.0/float64(den)) / 100
}

// medianPaise is the median of non-negative paise values: the middle value,
// or the half-up average of the two middles. Zero values with no values is 0.
func medianPaise(vals []int64) int64 {
	if len(vals) == 0 {
		return 0
	}
	cp := append([]int64(nil), vals...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	n := len(cp)
	if n%2 == 1 {
		return cp[n/2]
	}
	return (cp[n/2-1] + cp[n/2] + 1) / 2
}

// shares100 splits 100.00 across amounts by largest remainder, so the
// rounded shares always sum to exactly 100.00. A zero total yields zeros.
func shares100(amounts []int64) []float64 {
	out := make([]float64, len(amounts))
	var total int64
	for _, a := range amounts {
		total += a
	}
	if total <= 0 {
		return out
	}
	type frac struct {
		i int
		f float64
	}
	floors := make([]int64, len(amounts))
	fr := make([]frac, len(amounts))
	var floored int64
	for i, a := range amounts {
		exact := float64(a) * 10000.0 / float64(total)
		f := math.Floor(exact)
		floors[i] = int64(f)
		floored += int64(f)
		fr[i] = frac{i, exact - f}
	}
	rest := 10000 - floored
	sort.Slice(fr, func(i, j int) bool {
		if fr[i].f != fr[j].f {
			return fr[i].f > fr[j].f
		}
		return fr[i].i < fr[j].i
	})
	for k := int64(0); k < rest && k < int64(len(fr)); k++ {
		floors[fr[k].i]++
	}
	for i, h := range floors {
		out[i] = float64(h) / 100
	}
	return out
}

// --- Date helpers (pure; days are YYYY-MM-DD strings) ---

func parseDay(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

func formatDay(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// DaysBetween counts inclusive calendar days from a to b (b >= a).
func DaysBetween(a, b string) (int, error) {
	ta, err := parseDay(a)
	if err != nil {
		return 0, err
	}
	tb, err := parseDay(b)
	if err != nil {
		return 0, err
	}
	return int(tb.Sub(ta).Hours()/24) + 1, nil
}

// dayDiff counts whole days from a to b (b - a, may be negative).
func dayDiff(a, b string) (int, error) {
	ta, err := parseDay(a)
	if err != nil {
		return 0, err
	}
	tb, err := parseDay(b)
	if err != nil {
		return 0, err
	}
	return int(math.Round(tb.Sub(ta).Hours() / 24)), nil
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// monthCursor steps a YYYY-MM string forward one month.
func nextMonth(ym string) (string, error) {
	t, err := time.Parse("2006-01", ym)
	if err != nil {
		return "", err
	}
	return t.AddDate(0, 1, 0).Format("2006-01"), nil
}

// prorate rounds limitPaise * days/dim half-up to paise, in integer math.
func prorate(limitPaise int64, days, dim int) int64 {
	if dim <= 0 || days <= 0 {
		return 0
	}
	return (2*limitPaise*int64(days) + int64(dim)) / (2 * int64(dim))
}

// --- Period totals ---

// WindowTotals is the flat scalar P&L for one window. SavingsRatePct is nil
// (JSON null) when the window has no income — a rate over zero is undefined,
// not zero.
type WindowTotals struct {
	Days                 int
	RowsTotal            int
	RowsIncome           int
	RowsExpense          int
	RowsTransfer         int
	IncomePaise          int64
	ExpensePaise         int64
	NetPaise             int64
	SavingsRatePct       *float64
	AvgDailyExpensePaise int64
	AvgDailyIncomePaise  int64
	TransferCount        int
	TransferVolumePaise  int64
}

// PeriodTotals aggregates one window. Transfers never enter income/expense:
// they are counted only as transfer count/volume.
func (s *Store) PeriodTotals(ctx context.Context, start, end string, category, account *string) (*WindowTotals, error) {
	rows, _, err := s.Query(ctx, start, end, nil, category, account, 0, 0)
	if err != nil {
		return nil, err
	}
	days, err := DaysBetween(start, end)
	if err != nil {
		return nil, err
	}
	t := &WindowTotals{Days: days}
	for _, r := range rows {
		t.RowsTotal++
		paise := paiseOf(r["amount_paise"])
		switch r["type"] {
		case "income":
			t.RowsIncome++
			t.IncomePaise += paise
		case "expense":
			t.RowsExpense++
			t.ExpensePaise += paise
		case "transfer":
			t.RowsTransfer++
			t.TransferCount++
			t.TransferVolumePaise += paise
		}
	}
	t.NetPaise = t.IncomePaise - t.ExpensePaise
	if t.IncomePaise > 0 {
		sr := roundPct2(t.NetPaise, t.IncomePaise)
		t.SavingsRatePct = &sr
	}
	t.AvgDailyExpensePaise = avgPaise(t.ExpensePaise, days)
	t.AvgDailyIncomePaise = avgPaise(t.IncomePaise, days)
	return t, nil
}

// --- Category breakdown ---

// CategorySlice is one ranked category row.
type CategorySlice struct {
	Category    string
	AmountPaise int64
	Count       int
	SharePct    float64
	AvgPaise    int64
}

// BreakdownResult is one filtered category table plus its footer. The footer
// is computed over the returned rows — a subset filter's total is the
// subset's sum, never the window's. Shares are recomputed over the returned
// rows and sum to 100.00.
type BreakdownResult struct {
	Rows              []CategorySlice
	TotalAmountPaise  int64
	TotalCount        int
	CategoriesApplied []string
	AccountsApplied   []string
	SortBy            string
	Order             string
}

var breakdownCategorySorts = []string{"amount", "name", "count"}

// BreakdownByCategory ranks one side (expense|income) of the window.
// categories picks rows (empty = all); accounts scopes which accounts'
// rows contribute. Transfers never appear on either side.
func (s *Store) BreakdownByCategory(ctx context.Context, start, end, side string, categories []string, accountNames []string, sortBy, order string) (*BreakdownResult, error) {
	var ids []primitive.ObjectID
	if len(accountNames) > 0 {
		var err error
		if ids, err = s.resolveAccountIDs(ctx, accountNames); err != nil {
			return nil, err
		}
	}
	rows, err := s.scanWindow(ctx, start, end, []string{side}, categories, ids)
	if err != nil {
		return nil, err
	}
	byCat := map[string]*CategorySlice{}
	for _, r := range rows {
		g := byCat[r.Category]
		if g == nil {
			g = &CategorySlice{Category: r.Category}
			byCat[r.Category] = g
		}
		g.AmountPaise += r.AmountPaise
		g.Count++
	}
	out := &BreakdownResult{
		Rows:              []CategorySlice{},
		CategoriesApplied: append([]string{}, categories...),
		AccountsApplied:   append([]string{}, accountNames...),
		SortBy:            sortBy,
		Order:             order,
	}
	for _, g := range byCat {
		out.Rows = append(out.Rows, *g)
	}
	sortCategorySlices(out.Rows, sortBy, order)
	amounts := make([]int64, len(out.Rows))
	for i, g := range out.Rows {
		amounts[i] = g.AmountPaise
		out.TotalAmountPaise += g.AmountPaise
		out.TotalCount += g.Count
	}
	for i, sh := range shares100(amounts) {
		out.Rows[i].SharePct = sh
		out.Rows[i].AvgPaise = avgPaise(out.Rows[i].AmountPaise, out.Rows[i].Count)
	}
	return out, nil
}

// sortCategorySlices orders category rows by the caller's key. Every key
// keeps the name tie-break ascending, so the result is deterministic.
func sortCategorySlices(rows []CategorySlice, sortBy, order string) {
	keyLess := func(a, b CategorySlice) (bool, bool) {
		switch sortBy {
		case "name":
			return a.Category < b.Category, a.Category != b.Category
		case "count":
			if a.Count != b.Count {
				return a.Count < b.Count, true
			}
			return a.Category < b.Category, true
		default: // amount
			if a.AmountPaise != b.AmountPaise {
				return a.AmountPaise < b.AmountPaise, true
			}
			return a.Category < b.Category, true
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		less, decided := keyLess(rows[i], rows[j])
		if !decided {
			return false
		}
		if order == "asc" {
			return less
		}
		// desc flips value compares; the name key and every tie-break stay
		// ascending (keyLess already orders ties by name).
		if sortBy == "name" {
			return !less
		}
		a, b := rows[i], rows[j]
		switch sortBy {
		case "count":
			if a.Count != b.Count {
				return a.Count > b.Count
			}
		default:
			if a.AmountPaise != b.AmountPaise {
				return a.AmountPaise > b.AmountPaise
			}
		}
		return a.Category < b.Category
	})
}

// --- Account flow ---

// AccountFlowRow is one account's movement and closing balance.
type AccountFlowRow struct {
	ID               string
	Name             string
	Archived         bool
	OpeningPaise     int64
	InflowsPaise     int64
	OutflowsPaise    int64
	NetPaise         int64
	ClosingPaise     int64
	IncomePaise      int64
	ExpensePaise     int64
	TransferInPaise  int64
	TransferOutPaise int64
	RowsTotal        int
}

// AccountFlowResult is one account table plus its flat footer. The footer
// sums the returned rows; TotalActiveClosingPaise excludes archived rows.
type AccountFlowResult struct {
	Rows                     []AccountFlowRow
	TotalOpeningPaise        int64
	TotalInflowsPaise        int64
	TotalOutflowsPaise       int64
	TotalNetPaise            int64
	TotalClosingPaise        int64
	TotalActiveClosingPaise  int64
	TotalIncomePaise         int64
	TotalExpensePaise        int64
	TotalTransferVolumePaise int64
	AccountsApplied          []string
	CategoriesApplied        []string
	SortBy                   string
	Order                    string
}

var accountFlowSorts = []string{"closing", "name", "net", "inflows", "outflows"}

// AccountFlow reports per-account opening/in/out/net/closing for the window.
// Opening is always the account's real balance before the window (seed plus
// ALL signed legs strictly before start), and closing is always the real
// balance at the window end — the categories[] filter only narrows which
// window rows contribute to the flow columns (inflows/outflows, income,
// expense, transfer legs, rows), never the balances. Transfers carry their
// logged category, default other. accounts[] picks rows. An archived account
// with a zero seed and no rows ever is omitted.
func (s *Store) AccountFlow(ctx context.Context, start, end string, categories []string, accountNames []string, sortBy, order string) (*AccountFlowResult, error) {
	var filterIDs []primitive.ObjectID
	if len(accountNames) > 0 {
		var err error
		if filterIDs, err = s.resolveAccountIDs(ctx, accountNames); err != nil {
			return nil, err
		}
	}
	// Account directory: every account with its opening seed (changeable only
	// via edit_account).
	cur, err := s.accts.Find(ctx, bson.M{})
	if err != nil {
		return nil, err
	}
	var adocs []bson.M
	if err := cur.All(ctx, &adocs); err != nil {
		return nil, err
	}
	type acct struct {
		id       string
		name     string
		archived bool
		seed     int64
	}
	accts := make([]acct, 0, len(adocs))
	byID := map[string]int{}
	for _, a := range adocs {
		id := idHex(a["_id"])
		arch, _ := a["archived"].(bool)
		name, _ := a["name"].(string)
		byID[id] = len(accts)
		accts = append(accts, acct{id: id, name: name, archived: arch, seed: paiseOf(a["openingBalance"])})
	}
	// Pre-start legs: EVERY row dated before start, signed by account — never
	// category-scoped, so opening is the real balance, not a slice of it.
	preCur, err := s.txns.Find(ctx, bson.M{"date": bson.M{"$lt": start}, "deletedAt": bson.M{"$exists": false}})
	if err != nil {
		return nil, err
	}
	var preDocs []bson.M
	if err := preCur.All(ctx, &preDocs); err != nil {
		return nil, err
	}
	preLegs := map[string]int64{}
	preSeen := map[string]bool{}
	for _, d := range preDocs {
		for aid, delta := range signedPaise(d) {
			preLegs[aid] += delta
			preSeen[aid] = true
		}
	}
	// In-window legs, honoring the category filter for the flow columns.
	winRows, err := s.scanWindow(ctx, start, end, nil, categories, nil)
	if err != nil {
		return nil, err
	}
	// Real closing needs the UNSCOPED window net when a category filter is
	// set: closing = opening + all legs in the window, not just the matching
	// rows'. Only scanned when filtered — unfiltered, the scoped scan above
	// already sees every row.
	fullNet := map[string]int64{}
	if len(categories) > 0 {
		fullRows, err := s.scanWindow(ctx, start, end, nil, nil, nil)
		if err != nil {
			return nil, err
		}
		for _, r := range fullRows {
			switch r.Type {
			case "income":
				fullNet[r.AccountID] += r.AmountPaise
			case "expense":
				fullNet[r.AccountID] -= r.AmountPaise
			case "transfer":
				fullNet[r.AccountID] -= r.AmountPaise
				fullNet[r.SendingToID] += r.AmountPaise
			}
		}
	}
	winSeen := map[string]bool{}
	legs := map[string]struct {
		in, out, income, expense, tin, tout int64
		rows                                int
	}{}
	add := func(aid string, e struct {
		in, out, income, expense, tin, tout int64
		rows                                int
	}) {
		v := legs[aid]
		v.in += e.in
		v.out += e.out
		v.income += e.income
		v.expense += e.expense
		v.tin += e.tin
		v.tout += e.tout
		v.rows += e.rows
		legs[aid] = v
		winSeen[aid] = true
	}
	// Transfer volume counts each transfer ONCE when it touches a returned
	// account — not the leg sum halved. Halving is only correct when both
	// legs are returned; an accounts[] filter covering one side used to
	// report half the money moved.
	tvolSeen := map[string]int64{}
	wanted := map[string]bool{}
	for _, id := range filterIDs {
		wanted[id.Hex()] = true
	}
	touchesReturned := func(aid, sid string) bool {
		return len(wanted) == 0 || wanted[aid] || wanted[sid]
	}
	for _, r := range winRows {
		switch r.Type {
		case "income":
			add(r.AccountID, struct {
				in, out, income, expense, tin, tout int64
				rows                                int
			}{in: r.AmountPaise, income: r.AmountPaise, rows: 1})
		case "expense":
			add(r.AccountID, struct {
				in, out, income, expense, tin, tout int64
				rows                                int
			}{out: r.AmountPaise, expense: r.AmountPaise, rows: 1})
		case "transfer":
			add(r.AccountID, struct {
				in, out, income, expense, tin, tout int64
				rows                                int
			}{out: r.AmountPaise, tout: r.AmountPaise, rows: 1})
			add(r.SendingToID, struct {
				in, out, income, expense, tin, tout int64
				rows                                int
			}{in: r.AmountPaise, tin: r.AmountPaise, rows: 1})
			if touchesReturned(r.AccountID, r.SendingToID) {
				tvolSeen[r.ID] = r.AmountPaise
			}
		}
	}
	// A transfer touches two rows-entries (one per leg) but is one row; rows
	// counts each transfer once per account it touches.
	out := &AccountFlowResult{
		Rows:              []AccountFlowRow{},
		AccountsApplied:   append([]string{}, accountNames...),
		CategoriesApplied: append([]string{}, categories...),
		SortBy:            sortBy,
		Order:             order,
	}
	for _, a := range accts {
		if len(wanted) > 0 && !wanted[a.id] {
			continue
		}
		opening := a.seed + preLegs[a.id]
		l := legs[a.id]
		if a.archived && opening == 0 && !preSeen[a.id] && !winSeen[a.id] {
			continue
		}
		net := l.in - l.out
		closing := opening + net
		if len(categories) > 0 {
			closing = opening + fullNet[a.id]
		}
		row := AccountFlowRow{
			ID: a.id, Name: a.name, Archived: a.archived,
			OpeningPaise: opening,
			InflowsPaise: l.in, OutflowsPaise: l.out, NetPaise: net,
			ClosingPaise: closing,
			IncomePaise:  l.income, ExpensePaise: l.expense,
			TransferInPaise: l.tin, TransferOutPaise: l.tout,
			RowsTotal: l.rows,
		}
		out.Rows = append(out.Rows, row)
		out.TotalOpeningPaise += opening
		out.TotalInflowsPaise += l.in
		out.TotalOutflowsPaise += l.out
		out.TotalIncomePaise += l.income
		out.TotalExpensePaise += l.expense
		out.TotalClosingPaise += closing
		if !a.archived {
			out.TotalActiveClosingPaise += closing
		}
	}
	for _, v := range tvolSeen {
		out.TotalTransferVolumePaise += v
	}
	out.TotalNetPaise = out.TotalInflowsPaise - out.TotalOutflowsPaise
	sortAccountFlowRows(out.Rows, sortBy, order)
	return out, nil
}

func sortAccountFlowRows(rows []AccountFlowRow, sortBy, order string) {
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		var less bool
		tied := false
		switch sortBy {
		case "name":
			less = a.Name < b.Name
		case "net":
			if tied = (a.NetPaise == b.NetPaise); !tied {
				less = a.NetPaise < b.NetPaise
			} else {
				less = a.Name < b.Name
			}
		case "inflows":
			if tied = (a.InflowsPaise == b.InflowsPaise); !tied {
				less = a.InflowsPaise < b.InflowsPaise
			} else {
				less = a.Name < b.Name
			}
		case "outflows":
			if tied = (a.OutflowsPaise == b.OutflowsPaise); !tied {
				less = a.OutflowsPaise < b.OutflowsPaise
			} else {
				less = a.Name < b.Name
			}
		default: // closing
			if tied = (a.ClosingPaise == b.ClosingPaise); !tied {
				less = a.ClosingPaise < b.ClosingPaise
			} else {
				less = a.Name < b.Name
			}
		}
		if order == "asc" {
			return less
		}
		// desc flips value compares; name (the key and every tie-break)
		// stays ascending.
		if sortBy == "name" {
			return a.Name > b.Name
		}
		if tied {
			return a.Name < b.Name
		}
		return !less
	})
}

// --- Largest expenses ---

// TopExpensesResult is one bounded expense list. TotalMatching counts every
// expense in the window; the rows carry at most limit of them.
type TopExpensesResult struct {
	Rows          []TxRow
	TotalMatching int
	LimitApplied  int
	SortBy        string
	Order         string
}

var topExpensesSorts = []string{"amount", "date"}

// TopExpenses lists the largest (or, by sort, earliest) expense rows.
func (s *Store) TopExpenses(ctx context.Context, start, end string, category, account *string, limit int, sortBy, order string) (*TopExpensesResult, error) {
	rows, err := s.scanWindow(ctx, start, end, []string{"expense"}, nil, nil)
	if err != nil {
		return nil, err
	}
	// Narrow by the singular filters through the shared query path so the
	// semantics (either-side account match, normalized category) stay one.
	if category != nil || account != nil {
		allowed, _, err := s.Query(ctx, start, end, ptrString("expense"), category, account, 0, 0)
		if err != nil {
			return nil, err
		}
		keep := map[string]bool{}
		for _, r := range allowed {
			if id, ok := r["_id"].(string); ok {
				keep[id] = true
			}
		}
		filtered := rows[:0]
		for _, r := range rows {
			if keep[r.ID] {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if sortBy == "date" {
			if a.Date != b.Date {
				if order == "asc" {
					return a.Date < b.Date
				}
				return a.Date > b.Date
			}
		} else {
			if a.AmountPaise != b.AmountPaise {
				if order == "asc" {
					return a.AmountPaise < b.AmountPaise
				}
				return a.AmountPaise > b.AmountPaise
			}
			if a.Date != b.Date {
				return a.Date < b.Date
			}
		}
		if a.Date != b.Date {
			return a.Date < b.Date
		}
		return a.ID < b.ID
	})
	out := &TopExpensesResult{
		TotalMatching: len(rows),
		LimitApplied:  limit,
		SortBy:        sortBy,
		Order:         order,
		Rows:          []TxRow{},
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	out.Rows = append(out.Rows, rows...)
	return out, nil
}

func ptrString(s string) *string { return &s }

// --- Series ---

// Bucket is one time bucket's aggregates. From/To clip to the window; Days
// counts the clipped calendar days inside the window.
type Bucket struct {
	Key                  string
	From                 string
	To                   string
	Days                 int
	RowsTotal            int
	RowsIncome           int
	RowsExpense          int
	RowsTransfer         int
	IncomePaise          int64
	ExpensePaise         int64
	NetPaise             int64
	AvgDailyExpensePaise int64
	TransferCount        int
	TransferVolumePaise  int64
}

// Series buckets the window into day|week|month rows, zero-filled: every
// bucket in the grid appears even when it has no rows. Week buckets start
// Monday; the key is the canonical label (YYYY-MM-DD for days and Monday
// weeks, YYYY-MM for months).
func (s *Store) Series(ctx context.Context, start, end, bucket string, category, account *string, order string) ([]Bucket, error) {
	rows, _, err := s.Query(ctx, start, end, nil, category, account, 0, 0)
	if err != nil {
		return nil, err
	}
	scan := make([]TxRow, 0, len(rows))
	for _, r := range rows {
		var t TxRow
		if id, ok := r["_id"].(string); ok {
			t.ID = id
		}
		if d, ok := r["date"].(string); ok {
			t.Date = d
		}
		if ty, ok := r["type"].(string); ok {
			t.Type = ty
		}
		t.AmountPaise = paiseOf(r["amount_paise"])
		scan = append(scan, t)
	}
	grid, err := bucketGrid(start, end, bucket)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*Bucket{}
	for i := range grid {
		byKey[grid[i].Key] = &grid[i]
	}
	for _, r := range scan {
		key, err := bucketKey(r.Date, bucket)
		if err != nil {
			continue
		}
		b, ok := byKey[key]
		if !ok {
			continue
		}
		b.RowsTotal++
		switch r.Type {
		case "income":
			b.RowsIncome++
			b.IncomePaise += r.AmountPaise
		case "expense":
			b.RowsExpense++
			b.ExpensePaise += r.AmountPaise
		case "transfer":
			b.RowsTransfer++
			b.TransferCount++
			b.TransferVolumePaise += r.AmountPaise
		}
	}
	for i := range grid {
		grid[i].NetPaise = grid[i].IncomePaise - grid[i].ExpensePaise
		grid[i].AvgDailyExpensePaise = avgPaise(grid[i].ExpensePaise, grid[i].Days)
	}
	if order == "desc" {
		for i, j := 0, len(grid)-1; i < j; i, j = i+1, j-1 {
			grid[i], grid[j] = grid[j], grid[i]
		}
	}
	return grid, nil
}

// bucketKey maps a date to its canonical bucket label.
func bucketKey(date, bucket string) (string, error) {
	t, err := parseDay(date)
	if err != nil {
		return "", err
	}
	switch bucket {
	case "day":
		return formatDay(t), nil
	case "week":
		// Monday start: Sunday(0) goes back 6, else back wd-1.
		wd := int(t.Weekday())
		back := (wd + 6) % 7
		return formatDay(t.AddDate(0, 0, -back)), nil
	default: // month
		return t.Format("2006-01"), nil
	}
}

// bucketGrid builds the zero-filled bucket grid clipped to [start, end].
// Day grids over 500 buckets are rejected rather than truncated.
func bucketGrid(start, end, bucket string) ([]Bucket, error) {
	ts, err := parseDay(start)
	if err != nil {
		return nil, err
	}
	te, err := parseDay(end)
	if err != nil {
		return nil, err
	}
	out := []Bucket{}
	switch bucket {
	case "day":
		n := int(te.Sub(ts).Hours()/24) + 1
		if n > 500 {
			return nil, fail("%d buckets exceeds the 500-bucket limit — use week or month", n)
		}
		for d := ts; !d.After(te); d = d.AddDate(0, 0, 1) {
			k := formatDay(d)
			out = append(out, Bucket{Key: k, From: k, To: k, Days: 1})
		}
	case "week":
		wd := int(ts.Weekday())
		cur := ts.AddDate(0, 0, -((wd + 6) % 7))
		for !cur.After(te) {
			weekEnd := cur.AddDate(0, 0, 6)
			from := cur
			if from.Before(ts) {
				from = ts
			}
			to := weekEnd
			if to.After(te) {
				to = te
			}
			if !from.After(to) {
				days := int(to.Sub(from).Hours()/24) + 1
				out = append(out, Bucket{Key: formatDay(cur), From: formatDay(from), To: formatDay(to), Days: days})
			}
			cur = cur.AddDate(0, 0, 7)
		}
	default: // month
		cur := time.Date(ts.Year(), ts.Month(), 1, 0, 0, 0, 0, time.UTC)
		last := time.Date(te.Year(), te.Month(), 1, 0, 0, 0, 0, time.UTC)
		for !cur.After(last) {
			mEnd := time.Date(cur.Year(), cur.Month()+1, 0, 0, 0, 0, 0, time.UTC)
			from := cur
			if from.Before(ts) {
				from = ts
			}
			to := mEnd
			if to.After(te) {
				to = te
			}
			days := int(to.Sub(from).Hours()/24) + 1
			out = append(out, Bucket{Key: cur.Format("2006-01"), From: formatDay(from), To: formatDay(to), Days: days})
			cur = cur.AddDate(0, 1, 0)
		}
	}
	return out, nil
}

// --- Compare ---

// MetricDelta is one income|expense|net comparison row. DeltaPct is nil
// (JSON null) when the A side is zero and the delta moved — a percent over
// zero is undefined. When both sides are zero the delta is 0 and the percent
// is 0.00, not null.
type MetricDelta struct {
	Metric     string
	APaise     int64
	BPaise     int64
	DeltaPaise int64
	DeltaPct   *float64
	Direction  string
}

// CategoryDelta is one category's A-vs-B row.
type CategoryDelta struct {
	Category   string
	APaise     int64
	BPaise     int64
	DeltaPaise int64
	DeltaPct   *float64
	Direction  string
	ASharePct  float64
	BSharePct  float64
	ACount     int
	BCount     int
}

func directionOf(delta int64) string {
	switch {
	case delta > 0:
		return "up"
	case delta < 0:
		return "down"
	default:
		return "flat"
	}
}

// CompareTotals subtracts period A from period B for income, expense and net.
func (s *Store) CompareTotals(ctx context.Context, aStart, aEnd, bStart, bEnd string, category, account *string) ([]MetricDelta, error) {
	a, err := s.PeriodTotals(ctx, aStart, aEnd, category, account)
	if err != nil {
		return nil, err
	}
	b, err := s.PeriodTotals(ctx, bStart, bEnd, category, account)
	if err != nil {
		return nil, err
	}
	mk := func(name string, av, bv int64) MetricDelta {
		d := MetricDelta{Metric: name, APaise: av, BPaise: bv, DeltaPaise: bv - av, Direction: directionOf(bv - av)}
		if av != 0 {
			p := roundPct2(bv-av, av)
			d.DeltaPct = &p
		} else if bv-av == 0 {
			z := 0.0
			d.DeltaPct = &z
		}
		return d
	}
	// Identical windows are flat with 0.00 percents, per the plan: when both
	// sides are zero the delta is 0 and the percent is 0.00, not null.
	out := []MetricDelta{
		mk("income", a.IncomePaise, b.IncomePaise),
		mk("expense", a.ExpensePaise, b.ExpensePaise),
		mk("net", a.NetPaise, b.NetPaise),
	}
	return out, nil
}

// CompareCategories attributes the A-to-B change per category over expense
// rows. Only categories present on either side appear. The sort defaults to
// absolute delta descending; name and b_amount sorts are caller-chosen.
func (s *Store) CompareCategories(ctx context.Context, aStart, aEnd, bStart, bEnd string, categories []string, account *string, sortBy, order string) ([]CategoryDelta, error) {
	var ids []primitive.ObjectID
	if account != nil {
		var err error
		if ids, err = s.resolveAccountIDs(ctx, []string{*account}); err != nil {
			return nil, err
		}
	}
	aRows, err := s.scanWindow(ctx, aStart, aEnd, []string{"expense"}, categories, ids)
	if err != nil {
		return nil, err
	}
	bRows, err := s.scanWindow(ctx, bStart, bEnd, []string{"expense"}, categories, ids)
	if err != nil {
		return nil, err
	}
	type acc struct {
		aSum, bSum     int64
		aCount, bCount int
	}
	byCat := map[string]*acc{}
	for _, r := range aRows {
		g := byCat[r.Category]
		if g == nil {
			g = &acc{}
			byCat[r.Category] = g
		}
		g.aSum += r.AmountPaise
		g.aCount++
	}
	for _, r := range bRows {
		g := byCat[r.Category]
		if g == nil {
			g = &acc{}
			byCat[r.Category] = g
		}
		g.bSum += r.AmountPaise
		g.bCount++
	}
	var aTotal, bTotal int64
	for _, g := range byCat {
		aTotal += g.aSum
		bTotal += g.bSum
	}
	out := []CategoryDelta{}
	aAmounts := make([]int64, 0, len(byCat))
	bAmounts := make([]int64, 0, len(byCat))
	// Iterate in name order: shares100 breaks remainder ties by index, so a
	// map-ordered build would deal the extra 0.01 to a random category on
	// even splits. Name order first, caller sort after — like
	// BreakdownByCategory sorts before sharing.
	names := make([]string, 0, len(byCat))
	for cat := range byCat {
		names = append(names, cat)
	}
	sort.Strings(names)
	for _, cat := range names {
		g := byCat[cat]
		d := CategoryDelta{
			Category: cat, APaise: g.aSum, BPaise: g.bSum,
			DeltaPaise: g.bSum - g.aSum, Direction: directionOf(g.bSum - g.aSum),
			ACount: g.aCount, BCount: g.bCount,
		}
		if g.aSum != 0 {
			p := roundPct2(g.bSum-g.aSum, g.aSum)
			d.DeltaPct = &p
		} else if g.bSum-g.aSum == 0 {
			z := 0.0
			d.DeltaPct = &z
		}
		out = append(out, d)
		aAmounts = append(aAmounts, g.aSum)
		bAmounts = append(bAmounts, g.bSum)
	}
	// Shares use largest remainder so each window sums to exactly 100.00 —
	// independent rounding drifts to 99.99/100.01 on even splits.
	for i, sh := range shares100(aAmounts) {
		out[i].ASharePct = sh
	}
	for i, sh := range shares100(bAmounts) {
		out[i].BSharePct = sh
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		var less bool
		switch sortBy {
		case "name":
			less = a.Category < b.Category
		case "b_amount":
			if a.BPaise != b.BPaise {
				less = a.BPaise < b.BPaise
			} else {
				less = a.Category < b.Category
			}
		default: // delta: absolute delta, then name
			ad, bd := a.DeltaPaise, b.DeltaPaise
			if ad < 0 {
				ad = -ad
			}
			if bd < 0 {
				bd = -bd
			}
			if ad != bd {
				less = ad < bd
			} else {
				less = a.Category < b.Category
			}
		}
		if order == "asc" {
			return less
		}
		if sortBy == "name" {
			return !less && a.Category != b.Category
		}
		return !less
	})
	return out, nil
}

// --- Recurring ---

// RecurringCandidate is one detected repetition. Rows carry scalars only —
// occurrence rows are reachable through query_transactions by id.
type RecurringCandidate struct {
	Category           string
	Type               string
	AmountPaise        int64
	Count              int
	FirstDate          string
	LastDate           string
	MedianIntervalDays int
	IntervalJitterDays int
	Cadence            string
	NextExpectedDate   string
	Confidence         string
	Account            string
	AccountCount       int
}

var recurringSorts = []string{"next", "amount", "count"}

// cadenceFor maps a median interval in days to a human cadence band.
func cadenceFor(median int) string {
	switch {
	case median >= 5 && median <= 9:
		return "weekly"
	case median >= 12 && median <= 17:
		return "fortnightly"
	case median >= 25 && median <= 35:
		return "monthly"
	case median >= 80 && median <= 100:
		return "quarterly"
	default:
		return "irregular"
	}
}

// DetectRecurring groups rows by category+type and clusters near-equal
// amounts (within ±tolerance of the cluster median; 0 means exact). Only
// clusters with at least minOccurrences rows become candidates. Income is
// detected like expense; transfers never appear — the scan excludes them.
func (s *Store) DetectRecurring(ctx context.Context, start, end string, category, account *string, minOcc int, tolerancePct float64, limit int, sortBy, order string) ([]RecurringCandidate, int, error) {
	rows, err := s.scanWindow(ctx, start, end, []string{"income", "expense"}, nil, nil)
	if err != nil {
		return nil, 0, err
	}
	if category != nil || account != nil {
		allowed, _, err := s.Query(ctx, start, end, nil, category, account, 0, 0)
		if err != nil {
			return nil, 0, err
		}
		keep := map[string]bool{}
		for _, r := range allowed {
			if id, ok := r["_id"].(string); ok {
				keep[id] = true
			}
		}
		filtered := rows[:0]
		for _, r := range rows {
			if keep[r.ID] {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}
	groups := map[string][]TxRow{}
	for _, r := range rows {
		groups[r.Category+"\x00"+r.Type] = append(groups[r.Category+"\x00"+r.Type], r)
	}
	out := []RecurringCandidate{}
	for _, g := range groups {
		for _, cl := range clusterAmounts(g, tolerancePct) {
			if len(cl) < minOcc {
				continue
			}
			cand, ok := describeCluster(cl)
			if !ok {
				continue
			}
			out = append(out, cand)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		var less bool
		switch sortBy {
		case "amount":
			if a.AmountPaise != b.AmountPaise {
				less = a.AmountPaise < b.AmountPaise
			} else {
				less = a.Category < b.Category
			}
		case "count":
			if a.Count != b.Count {
				less = a.Count < b.Count
			} else {
				less = a.Category < b.Category
			}
		default: // next: expected date ascending, nulls last, then category
			if a.NextExpectedDate != b.NextExpectedDate {
				if a.NextExpectedDate == "" {
					less = false
				} else if b.NextExpectedDate == "" {
					less = true
				} else {
					less = a.NextExpectedDate < b.NextExpectedDate
				}
			} else {
				less = a.Category < b.Category
			}
		}
		if order == "asc" {
			return less
		}
		if sortBy == "next" {
			// desc flips dates (nulls first would be wrong — keep nulls last).
			if (a.NextExpectedDate == "") != (b.NextExpectedDate == "") {
				return b.NextExpectedDate == ""
			}
			if a.NextExpectedDate != b.NextExpectedDate {
				return a.NextExpectedDate > b.NextExpectedDate
			}
			return a.Category > b.Category
		}
		return !less
	})
	total := len(out)
	if len(out) > limit {
		out = out[:limit]
	}
	return out, total, nil
}

// clusterAmounts greedily groups date-sorted rows: each row joins the first
// cluster whose median is within ±tolerancePct, else starts a new one.
func clusterAmounts(rows []TxRow, tolerancePct float64) [][]TxRow {
	cp := append([]TxRow(nil), rows...)
	sort.SliceStable(cp, func(i, j int) bool {
		if cp[i].Date != cp[j].Date {
			return cp[i].Date < cp[j].Date
		}
		return cp[i].ID < cp[j].ID
	})
	clusters := [][]TxRow{}
	medians := []int64{}
	for _, r := range cp {
		placed := false
		for i, cl := range clusters {
			m := medians[i]
			var lo, hi int64
			if tolerancePct == 0 {
				lo, hi = m, m
			} else {
				delta := int64(math.Ceil(float64(m) * tolerancePct / 100.0))
				lo, hi = m-delta, m+delta
			}
			if r.AmountPaise >= lo && r.AmountPaise <= hi {
				clusters[i] = append(cl, r)
				amts := make([]int64, len(clusters[i]))
				for k, x := range clusters[i] {
					amts[k] = x.AmountPaise
				}
				medians[i] = medianPaise(amts)
				placed = true
				break
			}
		}
		if !placed {
			clusters = append(clusters, []TxRow{r})
			medians = append(medians, r.AmountPaise)
		}
	}
	kept := [][]TxRow{}
	for _, cl := range clusters {
		if len(cl) > 1 {
			kept = append(kept, cl)
		}
	}
	return kept
}

// describeCluster derives cadence evidence from one amount cluster. It
// reports false only when the cluster has no usable intervals.
func describeCluster(cl []TxRow) (RecurringCandidate, bool) {
	amts := make([]int64, len(cl))
	for i, r := range cl {
		amts[i] = r.AmountPaise
	}
	median := medianPaise(amts)
	intervals := []int{}
	for i := 1; i < len(cl); i++ {
		d, err := dayDiff(cl[i-1].Date, cl[i].Date)
		if err != nil || d < 0 {
			continue
		}
		intervals = append(intervals, d)
	}
	if len(intervals) == 0 {
		return RecurringCandidate{}, false
	}
	ivals := make([]int64, len(intervals))
	for i, d := range intervals {
		ivals[i] = int64(d)
	}
	medInterval := int(medianPaise(ivals))
	jitter := 0
	for _, d := range intervals {
		if j := d - medInterval; j < 0 {
			if -j > jitter {
				jitter = -j
			}
		} else if j > jitter {
			jitter = j
		}
	}
	cadence := cadenceFor(medInterval)
	conf := "low"
	if jitter <= 3 {
		conf = "high"
	} else if jitter <= 7 {
		conf = "medium"
	}
	next := ""
	if cadence != "irregular" {
		t, err := parseDay(cl[len(cl)-1].Date)
		if err == nil {
			next = formatDay(t.AddDate(0, 0, medInterval))
		}
	}
	accts := map[string]bool{}
	for _, r := range cl {
		accts[r.Account] = true
	}
	acct := ""
	if len(accts) == 1 {
		for _, r := range cl {
			acct = r.Account
			break
		}
	}
	return RecurringCandidate{
		Category: cl[0].Category, Type: cl[0].Type,
		AmountPaise: median, Count: len(cl),
		FirstDate: cl[0].Date, LastDate: cl[len(cl)-1].Date,
		MedianIntervalDays: medInterval, IntervalJitterDays: jitter,
		Cadence: cadence, NextExpectedDate: next, Confidence: conf,
		Account: acct, AccountCount: len(accts),
	}, true
}

// --- Anomalies ---

// BucketOutlier is one unusual period.
type BucketOutlier struct {
	Key          string
	From         string
	To           string
	Days         int
	ExpensePaise int64
	MedianPaise  int64
	DeltaPaise   int64
	Direction    string
}

// RowOutlier is one unusual transaction, carrying its row id so the caller
// can edit or delete it directly.
type RowOutlier struct {
	ID                  string
	Date                string
	AmountPaise         int64
	Category            string
	Account             string
	Note                string
	CategoryMedianPaise int64
	RatioPct            float64
	Direction           string
}

// BucketAnomalies flags buckets whose expense is unusual against the median
// bucket. Below-median drops are reported too. Fewer than 3 buckets is not
// an error — it yields an insufficient baseline and no outliers.
func (s *Store) BucketAnomalies(ctx context.Context, start, end, bucket string, category, account *string, sensitivity string, minAmountPaise int64, sortBy, order string) ([]BucketOutlier, Bucket, int, bool, error) {
	buckets, err := s.Series(ctx, start, end, bucket, category, account, "asc")
	if err != nil {
		return nil, Bucket{}, 0, false, err
	}
	if len(buckets) < 3 {
		base := Bucket{}
		if len(buckets) > 0 {
			exps := make([]int64, len(buckets))
			for i, b := range buckets {
				exps[i] = b.ExpensePaise
			}
			base.ExpensePaise = medianPaise(exps)
		}
		return []BucketOutlier{}, base, len(buckets), true, nil
	}
	exps := make([]int64, len(buckets))
	for i, b := range buckets {
		exps[i] = b.ExpensePaise
	}
	median := medianPaise(exps)
	ratio := SensitivityRatio(sensitivity)
	out := []BucketOutlier{}
	if median > 0 {
		for _, b := range buckets {
			var flag bool
			var dir string
			if float64(b.ExpensePaise) > ratio*float64(median) {
				flag, dir = true, "above"
			} else if b.ExpensePaise > 0 && b.ExpensePaise < median && float64(median) > ratio*float64(b.ExpensePaise) {
				// Below: median is more than ratio× the bucket. Zero-spend
				// buckets are never below-outliers — against any positive
				// median the ratio test on 0 is always true, so every quiet
				// day would report. A zero bucket is normal, not anomalous;
				// it still appears as a zero row in money_series.
				flag, dir = true, "below"
			}
			if !flag {
				continue
			}
			delta := b.ExpensePaise - median
			if delta < 0 {
				delta = -delta
			}
			if delta < minAmountPaise {
				continue
			}
			out = append(out, BucketOutlier{
				Key: b.Key, From: b.From, To: b.To, Days: b.Days,
				ExpensePaise: b.ExpensePaise, MedianPaise: median,
				DeltaPaise: b.ExpensePaise - median, Direction: dir,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if sortBy == "expense" {
			if out[i].ExpensePaise != out[j].ExpensePaise {
				if order == "asc" {
					return out[i].ExpensePaise < out[j].ExpensePaise
				}
				return out[i].ExpensePaise > out[j].ExpensePaise
			}
			return out[i].Key < out[j].Key
		}
		ai, bi := out[i].DeltaPaise, out[j].DeltaPaise
		if ai < 0 {
			ai = -ai
		}
		if bi < 0 {
			bi = -bi
		}
		if ai != bi {
			if order == "asc" {
				return ai < bi
			}
			return ai > bi
		}
		return out[i].Key < out[j].Key
	})
	return out, Bucket{ExpensePaise: median}, len(buckets), false, nil
}

// RowAnomalies flags single expense rows that dwarf their category median.
// Only overspend (above) is reported; income and transfers never appear.
// Categories with fewer than 3 rows are skipped, never guessed.
func (s *Store) RowAnomalies(ctx context.Context, start, end string, category, account *string, sensitivity string, minAmountPaise int64, sortBy, order string) ([]RowOutlier, int, int, error) {
	rows, err := s.scanWindow(ctx, start, end, []string{"expense"}, nil, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	if category != nil || account != nil {
		allowed, _, err := s.Query(ctx, start, end, ptrString("expense"), category, account, 0, 0)
		if err != nil {
			return nil, 0, 0, err
		}
		keep := map[string]bool{}
		for _, r := range allowed {
			if id, ok := r["_id"].(string); ok {
				keep[id] = true
			}
		}
		filtered := rows[:0]
		for _, r := range rows {
			if keep[r.ID] {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}
	byCat := map[string][]TxRow{}
	for _, r := range rows {
		byCat[r.Category] = append(byCat[r.Category], r)
	}
	ratio := SensitivityRatio(sensitivity)
	out := []RowOutlier{}
	evaluated, skipped := 0, 0
	for _, list := range byCat {
		if len(list) < 3 {
			skipped++
			continue
		}
		evaluated++
		amts := make([]int64, len(list))
		for i, r := range list {
			amts[i] = r.AmountPaise
		}
		median := medianPaise(amts)
		if median <= 0 {
			continue
		}
		for _, r := range list {
			if float64(r.AmountPaise) <= ratio*float64(median) {
				continue
			}
			if r.AmountPaise-median < minAmountPaise {
				continue
			}
			out = append(out, RowOutlier{
				ID: r.ID, Date: r.Date, AmountPaise: r.AmountPaise,
				Category: r.Category, Account: r.Account, Note: r.Note,
				CategoryMedianPaise: median,
				RatioPct:            roundPct2(r.AmountPaise, median),
				Direction:           "above",
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if sortBy == "ratio" {
			if out[i].RatioPct != out[j].RatioPct {
				if order == "asc" {
					return out[i].RatioPct < out[j].RatioPct
				}
				return out[i].RatioPct > out[j].RatioPct
			}
			return out[i].ID < out[j].ID
		}
		if out[i].AmountPaise != out[j].AmountPaise {
			if order == "asc" {
				return out[i].AmountPaise < out[j].AmountPaise
			}
			return out[i].AmountPaise > out[j].AmountPaise
		}
		return out[i].ID < out[j].ID
	})
	return out, evaluated, skipped, nil
}
