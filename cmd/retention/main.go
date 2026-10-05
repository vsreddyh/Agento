// Command retention enforces the Hermes bots' data lifecycle.
//
//	money        transactions autowiped when older than 90 days
//	health-check hc_meals + hc_days pruned after 30 days; hc_weight NEVER touched
//	cookbook     permanent — no-op
//	story/resumes git repos — no-op
//
// Only the remote MongoDB is touched (MONGODB_URI / MONGODB_DB from env).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"agento/internal/mongo"
	"agento/internal/tasks"

	"go.mongodb.org/mongo-driver/bson"
)

func run() int {
	dry := flag.Bool("dry-run", false, "report what would be removed without deleting anything")
	flag.Parse()

	if os.Getenv("MONGODB_URI") == "" {
		fmt.Println("[retention] MONGODB_URI not set — skipping.")
		return 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	d, err := mongo.DB()
	if err != nil {
		fmt.Printf("[retention] failed to connect to MongoDB: %v\n", err)
		return 1
	}
	today := time.Now().UTC()
	prefix := "[retention] "
	if *dry {
		prefix = "[retention] DRY-RUN: "
	}

	moneyCutoff := today.AddDate(0, 0, -90).Format("2006-01-02")
	moneyFilt := bson.M{"date": bson.M{"$lt": moneyCutoff}}
	n, err := d.Collection("money_transactions").CountDocuments(ctx, moneyFilt)
	if err != nil {
		fmt.Printf("[retention] money count failed: %v\n", err)
		return 1
	}
	if !*dry {
		if _, err := d.Collection("money_transactions").DeleteMany(ctx, moneyFilt); err != nil {
			fmt.Printf("[retention] money prune failed: %v\n", err)
			return 1
		}
	}
	fmt.Printf("%smoney: would remove %d transactions older than %s\n", prefix, n, moneyCutoff)

	foodCutoff := today.AddDate(0, 0, -30).Format("2006-01-02")
	for _, c := range []string{"hc_meals", "hc_days"} {
		filt := bson.M{"date": bson.M{"$lt": foodCutoff}}
		n, err := d.Collection(c).CountDocuments(ctx, filt)
		if err != nil {
			fmt.Printf("[retention] %s count failed: %v\n", c, err)
			return 1
		}
		if !*dry {
			if _, err := d.Collection(c).DeleteMany(ctx, filt); err != nil {
				fmt.Printf("[retention] %s prune failed: %v\n", c, err)
				return 1
			}
		}
		fmt.Printf("%shealth-check: would remove %d from %s older than %s\n", prefix, n, c, foodCutoff)
	}

	// Tasks: reconcile recurrence, do not prune (#180). This is the only thing in the
	// stack that notices a repeating task has stopped recurring. Rollover happens as
	// a side effect of Complete, so a writer that does not go through it — a script, a
	// migration, or a rollover that failed and was logged — stops the task forever
	// with no error a user would ever see. Read-only: it reports, and a human fixes
	// it, because a task that failed to roll over usually failed because its stored
	// data is wrong.
	reconcileTasks(ctx)

	fmt.Println("[retention] story/resumes/cookbook/projects: no retention policy (git repos / permanent).")
	return 0
}

// reconcileTasks reports completed structured repeats with no successor.
//
// Two deliberate choices, both about not letting a *report* break a job:
//
//   - Its OWN timeout, not the caller's — genuinely own, see the WithoutCancel note
//     below. Retention's 60s context is already eaten into by the money and health
//     deletes, and this pass walks up to ReconcileLimit candidates. Inheriting the
//     remainder meant a slow night produced "context deadline exceeded", so a problem
//     REPORT would have stopped tasks being pruned — and, since the reconciler was
//     read-only by design, it reported nothing at all rather than something partial.
//   - A failure here returns 0. Retention's contract is "prune what is due"; the
//     reconciliation is a diagnostic on top. If it cannot run, say so loudly on
//     stdout and let the prunes stand. Failing the job would suggest retention
//     itself is broken when it is not.
//
// Needs no new scheduler: retention is already the recurring job that owns data
// lifecycle, and a rollover failure is a lifecycle problem.
// reconcileBudget is the reconciler's context, extracted so the property that
// matters can be asserted without a database: it survives a parent that is ALREADY
// dead, because inheriting the caller's remainder is the bug it exists to prevent.
func reconcileBudget(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), reconcileBudgetFor)
}

// reconcileBudgetFor is the reconciler's own ceiling, independent of retention's.
const reconcileBudgetFor = 45 * time.Second

// It returns NOTHING, deliberately. It used to return an int that was always 0, which
// left `if code := reconcileTasks(ctx); code != 0 { return code }` at the call site — a
// branch no input could reach, inviting the next reader to hunt for the nonzero path
// that does not exist. The contract is stated in the comment above instead: this pass
// reports, and it can never fail the job.
func reconcileTasks(parent context.Context) {
	// WithoutCancel drops the parent's DEADLINE and cancellation but keeps its values,
	// so this really is an independent 45s rather than "45s, or whatever the caller
	// has left".
	//
	// It was WithTimeout(parent, 45s), which sounds like an own budget and is not:
	// the parent is retention's 60s, already partly spent on the money and health
	// deletes. On a slow night the reconciler inherited whatever was left — possibly
	// a second or nothing — and produced "context deadline exceeded", i.e. the exact
	// silent night this job exists to prevent, after the comment above already
	// claimed the opposite.
	//
	// The trade is deliberate: a cancelled retention run (an operator killing the
	// container) can now take up to 45s longer to exit, because this pass no longer
	// observes the cancellation. For a once-daily batch job that is much cheaper than
	// a repeat that silently stopped recurring.
	ctx, cancel := reconcileBudget(parent)
	defer cancel()

	if dl, ok := parent.Deadline(); ok && time.Until(dl) < reconcileBudgetFor {
		// Say so, rather than letting a starved report look like a healthy night.
		//
		// Clamped at zero: when the parent's budget is ALREADY gone, time.Until is
		// negative and the line read "had -5s left", which is not a thing and reads like
		// a clock bug in the job's own warning. The distinction that matters is only
		// "some left" versus "none left", so "0s left" says it.
		left := time.Until(dl)
		if left < 0 {
			left = 0
		}
		fmt.Printf("[retention] tasks: WARNING the job's own budget had %s left; "+
			"reconciliation runs on an independent %s\n",
			left.Round(time.Second), reconcileBudgetFor)
	}

	store, err := tasks.FromEnv()
	if err != nil {
		fmt.Printf("[retention] tasks: cannot connect, rollover gaps NOT reported: %v\n", err)
		return
	}
	res, err := store.ReconcileRollover(ctx)
	if err != nil {
		fmt.Printf("[retention] tasks: rollover reconciliation FAILED (%v); pruning is unaffected, but any stopped repeats are NOT listed this run\n", err)
		return
	}
	if res.Truncated {
		// Say so rather than letting an incomplete pass read as a clean one.
		fmt.Printf("[retention] tasks: WARNING only the %d most recent completions were checked; older ones are NOT covered this run.\n", tasks.ReconcileLimit)
	}
	if len(res.Gaps) == 0 {
		if !res.Truncated {
			fmt.Println("[retention] tasks: every repeating task rolled over correctly.")
		}
		return
	}
	fmt.Printf("[retention] tasks: %d repeating task(s) stopped recurring — each needs the next occurrence created by hand:\n", len(res.Gaps))
	for _, g := range res.Gaps {
		fmt.Printf("  - %s (%s) — %s\n", g.Name, g.TaskID, gapLine(g))
	}
}

// gapSeparator sits between the machine reason and the human-readable half of a gap
// line. Declared once, and matched by name in the tests rather than as a literal, so
// the two cannot drift.
const gapSeparator = " — "

// gapLine renders one gap, and CANNOT produce a line with no explanation.
//
// UserFacing() is empty for RolloverNone and RolloverCreated, which is correct in the
// contexts that use it — those outcomes need no explanation. Here every line is a
// "this needs a human" line, so an empty explanation would render as " - name (id) — "
// and leave the reader with a dangling dash and nothing to act on. Rather than trust
// the caller to have normalised the reason, the fallback states plainly that there is
// none, which is itself the information.
//
// Detail leads when present: it names the field to fix, while UserFacing() only ever
// described the OUTCOME, and making the reader parse a sentence about a category
// before learning which field was wrong is the wrong order.
func gapLine(g tasks.RolloverGap) string {
	outcome := g.Why.UserFacing()
	if outcome == "" {
		outcome = "no explanation is recorded for this outcome"
	}
	// Every line LEADS with the machine reason, then the human part.
	//
	// The reason goes first for two reasons that pull the same way. It makes each line
	// greppable by state, which it was not before: `exhausted` and `failed` have prose
	// from UserFacing(), so the fallback never fired and the string "exhausted" appeared
	// nowhere on the line — you could read why it stopped but not find every row in that
	// state. And it makes the three shapes uniform, so an operator reading a wall of
	// these sees the same shape every time instead of learning a new format per reason.
	//
	// Within the human part, Detail still leads: it names the field to fix, while
	// UserFacing() only ever described the outcome, and making someone parse a sentence
	// about a category before learning which field was wrong is the wrong order.
	body := outcome
	if g.Detail != "" {
		body = g.Detail + ": " + outcome
	}
	return string(g.Why) + gapSeparator + body
}

func main() { os.Exit(run()) }
