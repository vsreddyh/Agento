// Package mongo provides the shared MongoDB handle for all Go services.
// Env: MONGODB_URI (required), MONGODB_DB (default: hermes).
// Client + db are cached per process.
package mongo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

var (
	mu     sync.Mutex
	client *mongo.Client
	db     *mongo.Database
)

// DBName resolves the database name with the same default/fallback everywhere.
func DBName() string {
	if n := strings.TrimSpace(os.Getenv("MONGODB_DB")); n != "" {
		return n
	}
	return "hermes"
}

// Client returns the process-cached client, connecting on first use.
func Client() (*mongo.Client, error) {
	mu.Lock()
	defer mu.Unlock()
	if client != nil {
		return client, nil
	}
	uri := strings.TrimSpace(os.Getenv("MONGODB_URI"))
	if uri == "" {
		return nil, errors.New("MONGODB_URI is not set")
	}
	c, err := mongo.Connect(context.Background(), options.Client().
		ApplyURI(uri).
		SetServerSelectionTimeout(8*time.Second).
		SetRetryWrites(true))
	if err != nil {
		return nil, err
	}
	client = c
	return client, nil
}

// ConnectURI dials an explicit URI (for stores targeting a specific database,
// e.g. tests on a throwaway DB independent of process env).
func ConnectURI(uri string) (*mongo.Client, error) {
	return mongo.Connect(context.Background(), options.Client().
		ApplyURI(uri).
		SetServerSelectionTimeout(8*time.Second).
		SetRetryWrites(true))
}

// DB returns the process-cached database handle.
func DB() (*mongo.Database, error) {
	mu.Lock()
	defer mu.Unlock()
	if db != nil {
		return db, nil
	}
	mu.Unlock()
	c, err := Client()
	mu.Lock()
	if err != nil {
		return nil, err
	}
	if db == nil {
		db = c.Database(DBName())
	}
	return db, nil
}

// Col is a shortcut for DB()[name].
func Col(name string) (*mongo.Collection, error) {
	d, err := DB()
	if err != nil {
		return nil, err
	}
	return d.Collection(name), nil
}

// IDString renders a BSON id (ObjectID or string) as plain hex/string.
// NOTE: fmt.Sprint on an ObjectID yields ObjectID("...") — never use it.
func IDString(v any) string {
	if o, ok := v.(primitive.ObjectID); ok {
		return o.Hex()
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}
