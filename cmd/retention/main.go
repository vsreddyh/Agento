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
	if code := reconcileTasks(ctx); code != 0 {
		return code
	}

	fmt.Println("[retention] story/resumes/cookbook/projects: no retention policy (git repos / permanent).")
	return 0
}

// reconcileTasks reports completed structured repeats with no successor.
//
// Runs inside retention's existing 60s budget and needs no new scheduler: retention
// is already the recurring job that owns data lifecycle, and a rollover failure is
// a lifecycle problem — it is also what makes a task vanish three days later (#186).
func reconcileTasks(ctx context.Context) int {
	store, err := tasks.FromEnv()
	if err != nil {
		fmt.Printf("[retention] tasks: cannot connect: %v\n", err)
		return 1
	}
	gaps, err := store.ReconcileRollover(ctx)
	if err != nil {
		fmt.Printf("[retention] tasks: rollover reconciliation failed: %v\n", err)
		return 1
	}
	if len(gaps) == 0 {
		fmt.Println("[retention] tasks: every repeating task rolled over correctly.")
		return 0
	}
	fmt.Printf("[retention] tasks: %d repeating task(s) stopped recurring — each needs the next occurrence created by hand:\n", len(gaps))
	for _, g := range gaps {
		fmt.Printf("  - %s (%s) — %s\n", g.Name, g.TaskID, g.Why.UserFacing())
	}
	return 0
}

func main() { os.Exit(run()) }
