// Package mongo provides the shared MongoDB handle for all Go services.
// Env: MONGODB_URI (required), MONGODB_DB (default: hermes).
// Client + db are cached per process.
//
// One client per process is the granularity that exists: MCP servers are
// separate OS processes by protocol, so the stack legitimately holds ~20
// clients, and the pool caps below are what keep that from being expensive.
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

const (
	// defaultMaxPoolSize caps the driver's pool to EACH replica-set member
	// (the driver's own unit — it is per server, not per client).
	//
	// The driver default is 100 per member, which is dimensioning for load this
	// deployment will never see: two users, one assistant turn at a time, and
	// each server handles a single tool call sequentially. With ~20 processes
	// the default lets an idle stack hold thousands of connections, and the
	// 2026-10-31 outage showed why that matters — churn against that ceiling
	// (#423) is what took the cluster down, not steady-state count (#424).
	//
	// Invariant: nothing in this repo may fan out concurrent Mongo operations
	// within one process beyond this cap. Today the only parallel user is
	// pi-gateway's startup teach reconcile (one goroutine per profile, and there
	// is one profile). A new fan-out means revisiting this number first, because
	// the failure mode is silent queueing, not an error.
	defaultMaxPoolSize uint64 = 2

	// defaultMinPoolSize keeps no connections warm. Turns are separated by
	// seconds or minutes of the user reading, so a warm pool is idle capacity
	// that still counts against the cluster's connection limit.
	defaultMinPoolSize uint64 = 0
)

// clientOptions is the one option set every Mongo client in this repo starts
// from, so a default cannot be added to one path and forgotten on the other.
//
// Every default below is applied BEFORE ApplyURI, and that ordering is the
// whole contract: ApplyURI only overwrites a field the connection string
// actually names, so an explicit maxPoolSize / serverSelectionTimeoutMS /
// retryWrites in MONGODB_URI wins, while these values hold everywhere else.
// Splitting the chain around ApplyURI silently creates two classes of default;
// TestClientOptions* pins this.
func clientOptions(uri string) *options.ClientOptions {
	return options.Client().
		SetMaxPoolSize(defaultMaxPoolSize).
		SetMinPoolSize(defaultMinPoolSize).
		SetServerSelectionTimeout(8 * time.Second).
		SetRetryWrites(true).
		ApplyURI(uri)
}

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
	c, err := mongo.Connect(context.Background(), clientOptions(uri))
	if err != nil {
		return nil, err
	}
	client = c
	return client, nil
}

// ConnectURI dials an explicit URI (for stores targeting a specific database,
// e.g. tests on a throwaway DB independent of process env). It shares the pool
// defaults with Client(): a store that dials its own client is still one of the
// ~20 processes whose connections add up.
func ConnectURI(uri string) (*mongo.Client, error) {
	return mongo.Connect(context.Background(), clientOptions(uri))
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
