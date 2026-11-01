// Package mongo provides the shared MongoDB handle for all Go services.
// Env: MONGODB_URI (required), MONGODB_DB (default: hermes).
// Client + db are cached per process.
//
// The unit is one client per store, not per process: most stores dial their
// own client through mongostore.Open -> ConnectURI, so long-lived steady state
// is ~24 clients (~28 before the Stage 1 unwire of gmail/calendar) — 8 mcp-gate
// relays via approvals, 7 domain MCP children (browser holds none), 6 stores
// inside pi-gateway, 3 in health-api. One-shot dials (retention, cmd/mongo)
// are excluded from the count. The pool caps below are what keep that
// from being expensive.
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
	// each server handles a single tool call sequentially. With ~24 clients
	// the default lets an idle stack hold thousands of connections, and the
	// 2026-10-31 outage showed why that matters — churn against that ceiling
	// (#423) is what took the cluster down, not steady-state count (#424).
	//
	// Invariant: nothing in this repo may need more than 2 concurrent Mongo
	// operations per store. Today the only parallel user is pi-gateway's
	// startup teach reconcile (one goroutine per profile, and there is one
	// profile). Two things a future reader must not miss: with the default
	// primary read preference, reads and writes pin to the primary, so the
	// budget is 2 concurrent ops per store, not 2 per member; and concurrent
	// HTTP handlers against a shared store count as fan-out — a third
	// concurrent writer queues toward the 8s ServerSelectionTimeout, which
	// surfaces as an error, not just latency. money/tasks transactions hold a
	// session connection across multi-op bodies, so they spend the budget the
	// same way.
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
//
// The dial happens unlocked: holding mu across mongo.Connect would serialize
// every concurrent caller behind one handshake. Two racing first calls may
// both dial; the loser drops its spare client rather than leaking its
// monitors, and all later calls share the single published handle.
func Client() (*mongo.Client, error) {
	mu.Lock()
	if client != nil {
		c := client
		mu.Unlock()
		return c, nil
	}
	mu.Unlock()
	uri := strings.TrimSpace(os.Getenv("MONGODB_URI"))
	if uri == "" {
		return nil, errors.New("MONGODB_URI is not set")
	}
	c, err := mongo.Connect(context.Background(), clientOptions(uri))
	if err != nil {
		return nil, err
	}
	mu.Lock()
	if client != nil {
		spare := c
		c = client
		mu.Unlock()
		_ = spare.Disconnect(context.Background())
		return c, nil
	}
	client = c
	mu.Unlock()
	return c, nil
}

// ConnectURI dials an explicit URI (for stores targeting a specific database,
// e.g. tests on a throwaway DB independent of process env). It shares the pool
// defaults with Client(): a store that dials its own client is still one of the
// ~24 clients whose connections add up.
func ConnectURI(uri string) (*mongo.Client, error) {
	return mongo.Connect(context.Background(), clientOptions(uri))
}

// DB returns the process-cached database handle.
//
// Deliberately no defer here: the dial must happen unlocked (Client blocks on
// I/O), so every return path unlocks explicitly. A defer + manual Unlock mix
// is one early return away from a double-unlock panic; the discipline is that
// no path returns holding mu, whatever the count says today.
func DB() (*mongo.Database, error) {
	mu.Lock()
	if db != nil {
		d := db
		mu.Unlock()
		return d, nil
	}
	mu.Unlock()
	c, err := Client()
	if err != nil {
		return nil, err
	}
	mu.Lock()
	if db == nil {
		db = c.Database(DBName())
	}
	d := db
	mu.Unlock()
	return d, nil
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
