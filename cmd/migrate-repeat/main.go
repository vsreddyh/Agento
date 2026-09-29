// Command migrate-repeat converts the free-text repeat_rule of existing
// tasks into the structured recurrence introduced in 4.6 (repeat_every +
// repeat_unit, or repeat_custom + the words verbatim).
//
// Safety first, because this writes to live data:
//
//   - DRY RUN by default. Nothing is written without -apply.
//   - Only rules that match a known shape become structured. Anything else
//     (including counts outside 1-28) stays a custom condition with the
//     original words, so a rule like "mon-fri only" is never flattened into
//     a cadence that silently drops the exception.
//   - -backup writes the pre-image of every task it touches, so a bad run
//     can be reversed from the file.
//   - Idempotent: a task that already carries the structured keys is
//     skipped, so re-running is safe.
//
// Usage (from the repo root, with the root .env loaded):
//
//	go run ./cmd/migrate-repeat                      # report only
//	go run ./cmd/migrate-repeat -backup /tmp/x.json  # report + pre-image
//	go run ./cmd/migrate-repeat -apply -backup /tmp/x.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"agento/internal/mongo"
	"agento/internal/tasks"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func main() {
	apply := flag.Bool("apply", false, "write the changes (default: dry run)")
	backup := flag.String("backup", "", "write the pre-image of every touched task to this JSON file")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := run(ctx, *apply, *backup); err != nil {
		fmt.Fprintln(os.Stderr, "migrate-repeat:", err)
		os.Exit(1)
	}
}

// change is one task the migration would rewrite, with the pre-image kept
// for the backup file.
type change struct {
	ID     string         `json:"id"`
	Name   string         `json:"name"`
	Before map[string]any `json:"before"`
	After  tasks.Repeat   `json:"after"`
}

func run(ctx context.Context, apply bool, backupPath string) error {
	uri := strings.TrimSpace(os.Getenv("MONGODB_URI"))
	if uri == "" {
		return fmt.Errorf("MONGODB_URI is not set — load the root .env first")
	}
	dbName := strings.TrimSpace(os.Getenv("MONGODB_DB"))
	if dbName == "" {
		dbName = "hermes"
	}
	c, err := mongo.ConnectURI(uri)
	if err != nil {
		return err
	}
	coll := c.Database(dbName).Collection("tasks")

	cur, err := coll.Find(ctx, bson.M{})
	if err != nil {
		return err
	}
	defer cur.Close(ctx)

	var (
		changes  []change
		skipped  int
		oneShots int
	)
	for cur.Next(ctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			return err
		}
		oid, _ := doc["_id"].(primitive.ObjectID)
		rule, _ := doc["repeat_rule"].(string)
		// Already migrated (or a one-shot we deliberately left alone).
		if _, hasEvery := doc["repeat_every"]; hasEvery {
			skipped++
			continue
		}
		if strings.TrimSpace(rule) == "" {
			oneShots++
		}
		target := classify(rule)
		if target.IsZero() && strings.TrimSpace(rule) == "" {
			// Nothing to record: a one-shot already reads back as zero.
			continue
		}
		if err := target.Validate(); err != nil {
			return fmt.Errorf("task %s: refusing to write an invalid recurrence: %w", oid.Hex(), err)
		}
		name, _ := doc["name"].(string)
		before := map[string]any{
			"repeat_rule": rule,
		}
		if v, ok := doc["repeat_every"]; ok {
			before["repeat_every"] = v
		}
		changes = append(changes, change{
			ID:     oid.Hex(),
			Name:   name,
			Before: before,
			After:  target,
		})
	}
	if err := cur.Err(); err != nil {
		return err
	}

	sort.Slice(changes, func(i, j int) bool {
		if changes[i].After.Custom != changes[j].After.Custom {
			return !changes[i].After.Custom
		}
		return changes[i].Name < changes[j].Name
	})

	counts := map[string]int{}
	for _, ch := range changes {
		key := ch.After.String()
		counts[key]++
	}
	fmt.Printf("tasks scanned: %d to convert, %d one-shot, %d already migrated\n",
		len(changes), oneShots, skipped)
	for _, k := range sortedKeys(counts) {
		fmt.Printf("  %3d  %s\n", counts[k], k)
	}
	if len(changes) == 0 {
		return nil
	}
	fmt.Println()
	for _, ch := range changes {
		before, _ := ch.Before["repeat_rule"].(string)
		fmt.Printf("  %-24s %-22q -> %s\n", ch.Name, before, ch.After.String())
	}

	if backupPath != "" {
		if err := writeBackup(backupPath, changes); err != nil {
			return err
		}
		fmt.Printf("\npre-image written to %s\n", backupPath)
	}
	if !apply {
		fmt.Println("\ndry run: nothing written (pass -apply)")
		return nil
	}
	updated := 0
	for _, ch := range changes {
		oid, err := primitive.ObjectIDFromHex(ch.ID)
		if err != nil {
			return err
		}
		if _, err := coll.UpdateOne(ctx,
			bson.M{"_id": oid},
			bson.M{"$set": repeatSet(ch.After)},
		); err != nil {
			return fmt.Errorf("update %s: %w", ch.ID, err)
		}
		updated++
	}
	fmt.Printf("updated %d tasks\n", updated)
	return nil
}

// repeatSet renders the stored keys for a recurrence.
func repeatSet(rep tasks.Repeat) bson.M {
	rep = rep.Normalize()
	return bson.M{
		"repeat_every":  rep.Every,
		"repeat_unit":   rep.Unit,
		"repeat_custom": rep.Custom,
		"repeat_rule":   rep.Text,
	}
}

// "every N unit" is the only pattern that becomes a cadence, and only
// while N fits 1-28. Everything else — including a count outside the
// range — keeps the user's words, because a custom condition that says
// "every 12 days" is honest where a clamped "every 28 days" is a lie.
var everyN = regexp.MustCompile(`(?i)^every\s+(\d{1,3})\s+(day|week|month|year)s?$`)

// classify maps a legacy free-text rule onto the recurrence to store.
// Whitelist, not a loose regex: a rule carrying an exception ("daily, skip
// Wednesdays") must never be flattened into a plain cadence.
func classify(rule string) tasks.Repeat {
	rule = strings.TrimSpace(rule)
	if rule == "" {
		return tasks.Repeat{}
	}
	lowered := strings.ToLower(rule)
	for _, word := range []struct {
		text string
		unit string
	}{
		{"daily", "days"},
		{"weekly", "weeks"},
		{"monthly", "months"},
		{"yearly", "years"},
		{"annually", "years"},
	} {
		if lowered == word.text {
			return tasks.Repeat{Every: 1, Unit: word.unit}
		}
	}
	if m := everyN.FindStringSubmatch(lowered); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil && n >= tasks.RepeatEveryMin && n <= tasks.RepeatEveryMax {
			return tasks.Repeat{Every: n, Unit: m[2] + "s"}
		}
	}
	// Anything else stays exactly as written.
	return tasks.Repeat{Custom: true, Text: rule}
}

func writeBackup(path string, changes []change) error {
	raw := make([]map[string]any, 0, len(changes))
	for _, ch := range changes {
		raw = append(raw, map[string]any{
			"id":     ch.ID,
			"name":   ch.Name,
			"before": ch.Before,
		})
	}
	b, err := json.MarshalIndent(map[string]any{
		"collection": "tasks",
		"writtenAt":  time.Now().UTC().Format(time.RFC3339),
		"tasks":      raw,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
