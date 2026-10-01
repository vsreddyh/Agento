// Package mongostore holds the one shape every Mongo-backed domain
// store shares: connecting with the single-root-.env convention,
// tolerant field readers, and the list cap policy (#171).
//
// What it deliberately does NOT unify: cookbook/healthcheck build
// multi-collection stores from mongo.DB() (env-internal constructors —
// a different shape, left alone), and health-api's taskInt stays
// stricter than ToInt on purpose (fractionals fail at the HTTP door
// rather than truncate in the store).
package mongostore

import (
	"errors"
	"os"
	"strings"

	"agento/internal/mongo"

	mongoDrv "go.mongodb.org/mongo-driver/mongo"
)

// DefaultDB is the database when MONGODB_DB is unset (single root .env).
const DefaultDB = "hermes"

// Open connects with the shared convention: a blank URI is a hard error
// (MongoDB is the only backend), a blank db name falls back to DefaultDB.
func Open(uri, dbName string) (*mongoDrv.Client, *mongoDrv.Database, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return nil, nil, errors.New("MONGODB_URI is not set — MongoDB is the only backend")
	}
	if strings.TrimSpace(dbName) == "" {
		dbName = DefaultDB
	}
	c, err := mongo.ConnectURI(uri)
	if err != nil {
		return nil, nil, err
	}
	return c, c.Database(dbName), nil
}

// Env reads the pair every FromEnv passes through.
func Env() (uri, db string) {
	return os.Getenv("MONGODB_URI"), os.Getenv("MONGODB_DB")
}

// ToInt reads tolerant ints: the driver returns int32 for int32 fields,
// so a reader missing that case silently drops the value (#171). Floats
// truncate (3.5 -> 3); layers that must reject fractionals check first.
func ToInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// ToBool accepts real booleans only — strings like "true" are caller bugs,
// not values.
func ToBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}

// ClampLimit applies the list cap policy: limit <= 0 means the domain
// default, anything above the ceiling clamps (never rejected, so a stale
// caller gets fewer rows rather than an error).
func ClampLimit(limit, def, max int) int64 {
	if limit <= 0 {
		return int64(def)
	}
	if limit > max {
		return int64(max)
	}
	return int64(limit)
}
