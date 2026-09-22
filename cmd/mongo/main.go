// Command mongo is the remote-MongoDB helper for the Hermes bots.
//
// Usage:
//
//	mongo get <collection> [filter-json]
//	mongo count <collection> [filter-json]
//	mongo insert <collection> <doc-json>
//	mongo insert-many <collection> <docs-json-array>
//	mongo upsert <collection> <filter-json> <update-json>
//	mongo delete <collection> <filter-json>
//	mongo drop <collection>
//	mongo aggregate <collection> <pipeline-json>
//
// Environment: MONGODB_URI (required), MONGODB_DB (default: hermes).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"agento/internal/mongo"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func fail(msg string, code int) int {
	fmt.Fprintln(os.Stderr, msg)
	return code
}

func loadJSON(raw string) (any, int) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		fmt.Fprintf(os.Stderr, "invalid JSON: %v\n", err)
		return nil, 3
	}
	return v, 0
}

func toDoc(v any) (bson.M, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	return bson.M(m), true
}

func stringifyIDs(docs []bson.M) {
	for _, d := range docs {
		if id, ok := d["_id"]; ok {
			switch id.(type) {
			case string, int32, int64, float64, bool:
			default:
				d["_id"] = mongo.IDString(id)
			}
		}
	}
}

func run() int {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: mongo <get|count|insert|insert-many|upsert|delete|drop|aggregate> <collection> [json...]")
		return 2
	}
	cmd, collection, args := os.Args[1], os.Args[2], os.Args[3:]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	d, err := mongo.DB()
	if err != nil {
		return fail("MONGODB_URI is not set", 2)
	}
	col := d.Collection(collection)
	out := func(v any) int {
		b, _ := json.Marshal(v)
		fmt.Println(string(b))
		return 0
	}

	switch cmd {
	case "get", "count":
		var filt any = bson.M{}
		if len(args) > 0 {
			var code int
			if filt, code = loadJSON(args[0]); code != 0 {
				return code
			}
		}
		fm, ok := toDoc(filt)
		if !ok {
			return fail("filter must be a JSON object", 3)
		}
		if cmd == "count" {
			n, err := col.CountDocuments(ctx, fm)
			if err != nil {
				return fail(err.Error(), 1)
			}
			return out(bson.M{"count": n})
		}
		limit := int64(1000)
		if v := strings.TrimSpace(os.Getenv("MONGO_LIMIT")); v != "" {
			var n int64
			if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 {
				limit = n
			}
		}
		cur, err := col.Find(ctx, fm, options.Find().SetLimit(limit))
		if err != nil {
			return fail(err.Error(), 1)
		}
		var docs []bson.M
		if err := cur.All(ctx, &docs); err != nil {
			return fail(err.Error(), 1)
		}
		if docs == nil {
			docs = []bson.M{}
		}
		stringifyIDs(docs)
		return out(docs)

	case "insert":
		if len(args) < 1 {
			return fail("insert requires a doc JSON", 2)
		}
		v, code := loadJSON(args[0])
		if code != 0 {
			return code
		}
		doc, ok := toDoc(v)
		if !ok {
			return fail("insert requires a JSON object", 3)
		}
		res, err := col.InsertOne(ctx, doc)
		if err != nil {
			return fail(err.Error(), 1)
		}
		return out(bson.M{"inserted_id": mongo.IDString(res.InsertedID)})

	case "insert-many":
		if len(args) < 1 {
			return fail("insert-many requires a JSON array of docs", 2)
		}
		v, code := loadJSON(args[0])
		if code != 0 {
			return code
		}
		arr, ok := v.([]any)
		if !ok {
			return fail("insert-many expects a JSON array", 3)
		}
		docs := make([]any, 0, len(arr))
		for _, e := range arr {
			m, ok := toDoc(e)
			if !ok {
				return fail("insert-many expects an array of objects", 3)
			}
			docs = append(docs, m)
		}
		res, err := col.InsertMany(ctx, docs)
		if err != nil {
			return fail(err.Error(), 1)
		}
		ids := make([]string, 0, len(res.InsertedIDs))
		for _, id := range res.InsertedIDs {
			ids = append(ids, mongo.IDString(id))
		}
		return out(bson.M{"inserted_ids": ids})

	case "upsert":
		if len(args) < 2 {
			return fail("upsert requires <filter-json> <update-json>", 2)
		}
		fv, code := loadJSON(args[0])
		if code != 0 {
			return code
		}
		uv, code := loadJSON(args[1])
		if code != 0 {
			return code
		}
		fm, ok := toDoc(fv)
		if !ok {
			return fail("filter must be a JSON object", 3)
		}
		um, ok := toDoc(uv)
		if !ok {
			return fail("update must be a JSON object", 3)
		}
		res, err := col.UpdateOne(ctx, fm, bson.M{"$set": um}, options.Update().SetUpsert(true))
		if err != nil {
			return fail(err.Error(), 1)
		}
		upserted := ""
		if res.UpsertedID != nil {
			upserted = mongo.IDString(res.UpsertedID)
		}
		return out(bson.M{"matched": res.MatchedCount, "upserted": upserted})

	case "delete":
		if len(args) < 1 {
			return fail("delete requires a filter JSON", 2)
		}
		v, code := loadJSON(args[0])
		if code != 0 {
			return code
		}
		fm, ok := toDoc(v)
		if !ok {
			return fail("filter must be a JSON object", 3)
		}
		res, err := col.DeleteMany(ctx, fm)
		if err != nil {
			return fail(err.Error(), 1)
		}
		return out(bson.M{"deleted": res.DeletedCount})

	case "drop":
		if err := col.Drop(ctx); err != nil {
			return fail(err.Error(), 1)
		}
		return out(bson.M{"dropped": collection})

	case "aggregate":
		if len(args) < 1 {
			return fail("aggregate requires a pipeline JSON array", 2)
		}
		v, code := loadJSON(args[0])
		if code != 0 {
			return code
		}
		arr, ok := v.([]any)
		if !ok {
			return fail("aggregate expects a pipeline JSON array", 3)
		}
		pipe := make([]bson.M, 0, len(arr))
		for _, e := range arr {
			m, ok := toDoc(e)
			if !ok {
				return fail("pipeline stages must be objects", 3)
			}
			pipe = append(pipe, m)
		}
		cur, err := col.Aggregate(ctx, pipe)
		if err != nil {
			return fail(err.Error(), 1)
		}
		var docs []bson.M
		if err := cur.All(ctx, &docs); err != nil {
			return fail(err.Error(), 1)
		}
		if docs == nil {
			docs = []bson.M{}
		}
		stringifyIDs(docs)
		return out(docs)
	}
	return fail("unknown command: "+cmd, 2)
}

func main() { os.Exit(run()) }
