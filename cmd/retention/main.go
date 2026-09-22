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

	fmt.Println("[retention] story/resumes/cookbook: no retention policy (git repos / permanent).")
	return 0
}

func main() { os.Exit(run()) }
