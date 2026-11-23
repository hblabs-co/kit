package mongo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	mdb "go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"

	"github.com/hblabs-co/kit/core/datasources"
)

// defaultDatabase is the database name used when neither the URL
// nor MONGO_DATABASE specifies one. Keeping a sane default here
// means local dev works with just MONGO_URL=mongodb://localhost.
const defaultDatabase = "hblabs"

// kvCollection is the collection backing Datasource ops. Same name
// across services — entries are caller-scoped by key prefix, not by
// collection.
const kvCollection = "datasource_kv"

// defaultConnectTimeout caps the initial connection + Ping when
// Options.ConnectTimeout is zero. Long enough to absorb a slow DNS
// lookup or TLS handshake; short enough that a misconfigured cluster
// fails the boot fast instead of hanging forever.
const defaultConnectTimeout = 10 * time.Second

// kvTTLIndexName is the name of the index Mongo's TTL background sweep
// uses on the kv collection. Stable so re-running EnsureKVSchema is a
// no-op rather than creating a duplicate.
const kvTTLIndexName = "kv_ttl"

// indexAscending is Mongo's literal value for an ascending index on a
// field (the alternative is -1 for descending). Named here so the
// index spec reads as intent rather than as a stray 1.
const indexAscending = 1

// kvTTLExpireAfterSeconds = 0 tells Mongo to interpret the field's
// timestamp as the absolute expiry instant (rather than offsetting by
// N seconds). The driver's API takes a count of seconds; 0 is the
// well-known sentinel for this mode.
const kvTTLExpireAfterSeconds = 0

// Mongo is the typed client. Implements datasources.Datasource via
// a kv collection; exposes Client() and Database() for everything
// beyond opaque key/value (real domain repos, transactions,
// aggregations, change streams).
type Mongo struct {
	client *mdb.Client
	dbName string
}

// Options configures New / Open. Zero values pick safe defaults.
type Options struct {
	// URL is the mongodb:// (or mongodb+srv://) connection string.
	// Empty → the value of MONGO_URL.
	URL string

	// Database overrides the default database name. Empty → the
	// path component of URL, then MONGO_DATABASE, then "hblabs".
	Database string

	// ConnectTimeout caps the initial connection + Ping during Open.
	// 0 → defaultConnectTimeout.
	ConnectTimeout time.Duration

	// AppName surfaces in Mongo's currentOp output and metrics so
	// operators can attribute load to a specific service. Empty →
	// no app name is sent.
	AppName string
}

// New opens a Mongo client from environment variables (MONGO_URL,
// MONGO_DATABASE). Fails-fast when the URL is missing or the
// initial Ping doesn't succeed — the binary should refuse to start
// rather than serve traffic against a broken cluster.
func New(ctx context.Context) (*Mongo, error) {
	return Open(ctx, Options{})
}

// Open opens a Mongo client with explicit options.
func Open(ctx context.Context, opts Options) (*Mongo, error) {
	url := opts.URL
	if url == "" {
		url = strings.TrimSpace(os.Getenv(EnvURL))
	}
	if url == "" {
		return nil, fmt.Errorf("mongo: %s is unset and Options.URL is empty",
			EnvURL)
	}

	clientOpts := options.Client().ApplyURI(url)
	if opts.AppName != "" {
		clientOpts.SetAppName(opts.AppName)
	}

	connectTimeout := opts.ConnectTimeout
	if connectTimeout <= 0 {
		connectTimeout = defaultConnectTimeout
	}
	openCtx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	client, err := mdb.Connect(clientOpts)
	if err != nil {
		return nil, fmt.Errorf("mongo: connect: %w", err)
	}
	if err := client.Ping(openCtx, readpref.Primary()); err != nil {
		_ = client.Disconnect(context.Background())
		return nil, fmt.Errorf("mongo: initial ping: %w", err)
	}

	dbName := resolveDatabase(opts.Database, url)
	return &Mongo{client: client, dbName: dbName}, nil
}

// resolveDatabase picks the database name in priority order:
// explicit Options.Database → URL path → MONGO_DATABASE env →
// "hblabs" fallback. Centralised so the precedence is one place.
func resolveDatabase(explicit, url string) string {
	if explicit != "" {
		return explicit
	}
	if name := databaseFromURL(url); name != "" {
		return name
	}
	if env := strings.TrimSpace(os.Getenv(EnvDatabase)); env != "" {
		return env
	}
	return defaultDatabase
}

// databaseFromURL extracts the database name from the path
// component of a mongodb:// URL. Returns "" when the URL doesn't
// carry one ("mongodb://host:27017" or "mongodb://host/?opts...").
func databaseFromURL(url string) string {
	// Strip scheme.
	rest := url
	for _, scheme := range []string{"mongodb+srv://", "mongodb://"} {
		if r, ok := strings.CutPrefix(rest, scheme); ok {
			rest = r
			break
		}
	}
	// Drop the host[:port] portion.
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[i+1:]
	} else {
		return ""
	}
	// Drop any query string.
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSpace(rest)
}

// Client returns the underlying *mongo.Client for primitives the
// Datasource interface doesn't expose: collections, transactions,
// aggregations, change streams, watching for cluster events.
func (m *Mongo) Client() *mdb.Client { return m.client }

// Database returns the *mongo.Database the client is bound to.
// Equivalent to m.Client().Database(m.DatabaseName()), exposed as
// a one-liner because it's the most common access pattern.
func (m *Mongo) Database() *mdb.Database {
	return m.client.Database(m.dbName)
}

// DatabaseName returns the resolved database name.
func (m *Mongo) DatabaseName() string { return m.dbName }

// Collection returns the named collection on the bound database.
// Wrapping it once here means service code never repeats
// `m.Client().Database(name).Collection("...")`.
func (m *Mongo) Collection(name string) *mdb.Collection {
	return m.client.Database(m.dbName).Collection(name)
}

// Ping verifies the client can reach the primary. Suitable for
// observability.Healthz.
func (m *Mongo) Ping(ctx context.Context) error {
	return m.client.Ping(ctx, readpref.Primary())
}

// Close disconnects the client. After Close, every method returns
// errors from the driver.
func (m *Mongo) Close() error {
	return m.client.Disconnect(context.Background())
}

// EnsureKVSchema creates the TTL index on the kv collection. Call
// once at boot from any service that uses Datasource methods on
// this Mongo backend; the operation is idempotent so multiple
// services running it concurrently is fine.
//
// The index uses expireAfterSeconds: 0 — Mongo's background sweep
// deletes documents whose expires_at is in the past, ±60 seconds.
// Reads filter expired docs in the query (defence in depth) so a
// just-expired key never serves a stale value.
func (m *Mongo) EnsureKVSchema(ctx context.Context) error {
	coll := m.Collection(kvCollection)
	_, err := coll.Indexes().CreateOne(ctx, mdb.IndexModel{
		Keys:    map[string]any{"expires_at": indexAscending},
		Options: options.Index().SetExpireAfterSeconds(kvTTLExpireAfterSeconds).SetName(kvTTLIndexName),
	})
	if err != nil {
		return fmt.Errorf("mongo: ensure kv ttl index: %w", err)
	}
	return nil
}

// kvDocument is the on-wire shape of a kv entry. expires_at is
// optional (forever-stored entries omit it, so the TTL index
// doesn't sweep them).
type kvDocument struct {
	Key       string     `bson:"_id"`
	Value     []byte     `bson:"value"`
	ExpiresAt *time.Time `bson:"expires_at,omitempty"`
	UpdatedAt time.Time  `bson:"updated_at"`
}

// Get implements datasources.Datasource. Filters out documents
// whose expires_at is in the past so a sweep delay never surfaces
// expired bytes to the caller.
func (m *Mongo) Get(ctx context.Context, key string) ([]byte, error) {
	coll := m.Collection(kvCollection)
	now := time.Now()
	filter := map[string]any{
		"_id": key,
		"$or": []map[string]any{
			{"expires_at": map[string]any{"$exists": false}},
			{"expires_at": map[string]any{"$gt": now}},
		},
	}
	var doc kvDocument
	if err := coll.FindOne(ctx, filter).Decode(&doc); err != nil {
		if errors.Is(err, mdb.ErrNoDocuments) {
			return nil, datasources.ErrNotFound
		}
		return nil, fmt.Errorf("mongo: get %q: %w", key, err)
	}
	return doc.Value, nil
}

// Set implements datasources.Datasource. ttl=0 stores forever
// (omits expires_at, so the TTL index ignores the doc); negative
// ttl returns an error.
func (m *Mongo) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if ttl < 0 {
		return datasources.ErrNegativeTTL
	}
	coll := m.Collection(kvCollection)
	doc := kvDocument{
		Key:       key,
		Value:     value,
		UpdatedAt: time.Now(),
	}
	if ttl > 0 {
		t := time.Now().Add(ttl)
		doc.ExpiresAt = &t
	}
	_, err := coll.ReplaceOne(ctx,
		map[string]any{"_id": key},
		doc,
		options.Replace().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("mongo: set %q: %w", key, err)
	}
	return nil
}

// Delete implements datasources.Datasource. Idempotent: deleting a
// missing key is fine and reports zero deletions, which is success.
func (m *Mongo) Delete(ctx context.Context, key string) error {
	coll := m.Collection(kvCollection)
	if _, err := coll.DeleteOne(ctx, map[string]any{"_id": key}); err != nil {
		return fmt.Errorf("mongo: delete %q: %w", key, err)
	}
	return nil
}

// Exists implements datasources.Datasource. Same TTL filter as Get;
// a doc whose expires_at is in the past reads as absent.
func (m *Mongo) Exists(ctx context.Context, key string) (bool, error) {
	coll := m.Collection(kvCollection)
	now := time.Now()
	filter := map[string]any{
		"_id": key,
		"$or": []map[string]any{
			{"expires_at": map[string]any{"$exists": false}},
			{"expires_at": map[string]any{"$gt": now}},
		},
	}
	const existsLimit = 1 // we only need to know "any" — bail after the first hit
	n, err := coll.CountDocuments(ctx, filter, options.Count().SetLimit(existsLimit))
	if err != nil {
		return false, fmt.Errorf("mongo: exists %q: %w", key, err)
	}
	return n > 0, nil
}

// Ensure Mongo satisfies the umbrella interface at compile time.
var _ datasources.Datasource = (*Mongo)(nil)
