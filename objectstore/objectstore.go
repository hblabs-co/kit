// Package objectstore is the platform's typed handle on the
// MinIO / S3 cluster. It implements the small set of operations
// every service that puts bytes in object storage needs — open a
// client from the standard HB_MINIO_* env vars, make sure a bucket
// exists, PUT a stream under a key — and stays deliberately
// narrow: anything richer (signed URLs, multipart resumable, lifecycle
// rules) belongs to the caller's typed wrapper, not here.
//
// Why this lives under kit/. MinIO is the platform's
// only object-storage backend; conceptually it sits next to
// kit/mongo (typed wrapper over the document store)
// and the kv-style Datasource interface. The umbrella's doc.go calls
// this layout out: "one cluster for structured data, ephemeral
// state, and binary blobs". The `objectstore` name describes the
// purpose, not the implementation — if MinIO is ever swapped for a
// different S3-compatible backend the package name still fits.
//
// What this package is NOT:
//
//   - It is not the file-fingerprinting + manifest-emitting tool
//     that seed-time storage-sync runs. That logic
//     (services/service-tools/cmd/storage-sync) deliberately stays
//     in the tool — it owns a different workflow (sync from disk,
//     compute hash, emit _manifest.json) and bundling it here would
//     pull the seed-tool's concerns into every service that just
//     wants to PUT bytes.
//   - It is not the per-site asset-URL rewriter (that lives in
//     web/siteassets and consumes the manifests produced
//     by storage-sync). Same separation: the rewriter is a read
//     path; this package is a write path.
//
// Typical usage from a service's main.go:
//
//	cfg, err := objectstore.LoadConfig()
//	if err != nil { ... }
//	cfg.Bucket = envcfg.String(constants.EnvIngestRawBucket, "ingest-raw")
//	cli, err := objectstore.New(cfg)
//	if err != nil { ... }
//	if err := cli.EnsureBucket(ctx); err != nil { ... }
//
// Then handlers / workers take *Client (or the narrower Putter
// interface) by parameter so tests can fake the I/O surface.
package objectstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/hblabs-co/kit/core/constants"
	"github.com/hblabs-co/kit/core/envcfg"
)

// defaultRegion mirrors MinIO's own fallback. AWS-style backends
// route by this label; MinIO accepts anything and stores it as a
// literal. "us-east-1" works for both flavours.
const defaultRegion = "us-east-1"

// Config is the resolved connection settings the client needs.
// Kept as a value type with public fields so tests can fabricate
// one inline without exporting a constructor for every knob, and
// so the typical "load from env, then override Bucket" pattern is
// one line in main.go.
type Config struct {
	// Endpoint is the host[:port] (no scheme) the SDK connects to.
	// Scheme is selected by UseSSL. Read from HB_MINIO_ENDPOINT by
	// LoadConfig.
	Endpoint string

	// AccessKey / SecretKey are the S3 v4 static credentials.
	// Both required; LoadConfig refuses to return a Config with
	// either empty.
	AccessKey string
	SecretKey string

	// Region is the S3 region label sent in signed requests.
	// MinIO ignores the value but still requires one; AWS-style
	// backends pick routing from it. Empty → defaultRegion.
	Region string

	// UseSSL toggles https:// vs http:// at the transport. Local
	// compose runs plain http; remote endpoints require TLS.
	UseSSL bool

	// Bucket is the default bucket Client operations target. NOT
	// populated by LoadConfig because the bucket-policy decision
	// (which env var, which default) is the caller's, not the
	// package's. main.go usually sets this right after LoadConfig.
	Bucket string
}

// LoadConfig pulls the platform's standard HB_MINIO_* env vars
// (endpoint, credentials, region, SSL toggle) and returns a Config
// ready to hand to New — modulo Bucket, which the caller fills in
// from whatever env var fits its workflow (HB_MINIO_BUCKET for
// public assets, HB_INGEST_RAW_BUCKET for the crawler, ...).
//
// Endpoint normalisation: if HB_MINIO_ENDPOINT carries a
// http:// or https:// prefix (the convention deployment/infra/
// minio/install.sh writes), the scheme wins over HB_MINIO_USE_SSL
// and the Endpoint field is rewritten to the bare host[:port]
// the minio-go SDK expects. So an in-cluster value like
// `http://minio.storage.svc.cluster.local:9000` Just Works whether
// or not HB_MINIO_USE_SSL is set — and a misconfigured pair
// (scheme says http, flag says true) can't drift silently.
//
// Returns an error rather than panicking so the caller's main can
// decide the exit code. The three required fields (endpoint +
// both credentials) are validated up front; missing values fail
// loud at boot rather than silently degrading at the first PUT.
func LoadConfig() (Config, error) {
	cfg := Config{
		Endpoint:  envcfg.String(constants.EnvMinIOEndpoint, ""),
		AccessKey: envcfg.String(constants.EnvMinIOAccessKey, ""),
		SecretKey: envcfg.String(constants.EnvMinIOSecretKey, ""),
		Region:    envcfg.String(constants.EnvMinIORegion, defaultRegion),
		UseSSL:    envcfg.Bool(constants.EnvMinIOUseSSL, false),
	}
	if cfg.Endpoint == "" {
		return cfg, fmt.Errorf("objectstore: %s is unset", constants.EnvMinIOEndpoint)
	}
	if err := normaliseEndpoint(&cfg); err != nil {
		return cfg, err
	}
	if cfg.AccessKey == "" || cfg.SecretKey == "" {
		return cfg, fmt.Errorf("objectstore: %s and %s must both be set",
			constants.EnvMinIOAccessKey, constants.EnvMinIOSecretKey)
	}
	return cfg, nil
}

// normaliseEndpoint rewrites cfg.Endpoint when the operator
// supplied a full URL ("http://host" / "https://host:port"): the
// scheme drives cfg.UseSSL and the Endpoint is reduced to the
// bare host[:port] the minio-go SDK wants. Operators who supply a
// bare host:port keep whatever HB_MINIO_USE_SSL says.
//
// Mirrors services/service-tools/cmd/storage-sync/client.go's
// applyEndpointOverride — kept here too so every consumer of
// objectstore gets the same behaviour without re-implementing it.
func normaliseEndpoint(cfg *Config) error {
	if !strings.Contains(cfg.Endpoint, "://") {
		return nil
	}
	u, err := url.Parse(cfg.Endpoint)
	if err != nil {
		return fmt.Errorf("objectstore: parse %s %q: %w",
			constants.EnvMinIOEndpoint, cfg.Endpoint, err)
	}
	switch u.Scheme {
	case "https":
		cfg.UseSSL = true
	case "http":
		cfg.UseSSL = false
	default:
		return fmt.Errorf("objectstore: %s scheme must be http or https, got %q",
			constants.EnvMinIOEndpoint, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("objectstore: %s %q has no host",
			constants.EnvMinIOEndpoint, cfg.Endpoint)
	}
	cfg.Endpoint = u.Host
	return nil
}

// Putter is the minimal write surface callers should depend on
// when they only need to upload bytes. Taking a Putter rather than
// a *Client in handler/worker signatures keeps the test surface
// tiny — a fake with three lines is enough.
type Putter interface {
	Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
}

// Client is the typed handle over the underlying minio.Client.
// One instance per process is the expected shape; the SDK type
// is goroutine-safe so handlers and background workers can share
// it freely.
type Client struct {
	c      *minio.Client
	bucket string
	region string
}

// New opens a MinIO client against the configured endpoint. Fails
// fast on a missing bucket (the most common copy-paste mistake at
// the call site) rather than deferring to the first operation.
func New(cfg Config) (*Client, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("objectstore: Config.Bucket is empty")
	}
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("objectstore: minio client: %w", err)
	}
	return &Client{c: c, bucket: cfg.Bucket, region: cfg.Region}, nil
}

// Bucket is the resolved bucket name. Exposed so a log line can
// include it without re-reading env vars at the call site.
func (c *Client) Bucket() string { return c.bucket }

// EnsureBucket creates the bucket if it doesn't already exist.
// Idempotent: a second call is a no-op on the server, and a race
// between two replicas creating the bucket at the same instant is
// swallowed (MinIO returns BucketAlreadyOwnedByYou which we treat
// as success).
//
// Used at boot so a fresh cluster comes up without an out-of-band
// `mc mb` step. Services that boot against a long-lived bucket
// can call this at every start safely — the round-trip cost is
// one HEAD-like request.
func (c *Client) EnsureBucket(ctx context.Context) error {
	exists, err := c.c.BucketExists(ctx, c.bucket)
	if err != nil {
		return fmt.Errorf("objectstore: bucket exists %q: %w", c.bucket, err)
	}
	if exists {
		return nil
	}
	if err := c.c.MakeBucket(ctx, c.bucket, minio.MakeBucketOptions{Region: c.region}); err != nil {
		errResp := minio.ToErrorResponse(err)
		if errResp.Code == "BucketAlreadyOwnedByYou" || errResp.Code == "BucketAlreadyExists" {
			return nil
		}
		return fmt.Errorf("objectstore: make bucket %q: %w", c.bucket, err)
	}
	return nil
}

// RemoveAllUnderPrefix deletes every object whose key starts
// with `prefix` from the configured bucket. Used by the
// service-ingest GC to purge a site's MinIO directory after a
// soft-delete tombstone has aged past its grace period.
//
// Streams List → RemoveObjects through the minio-go channel API
// so a prefix with thousands of objects doesn't blow up memory.
// The first removal error (if any) is returned; subsequent
// errors on the same channel are dropped after the first, on the
// assumption that a transient backend issue affects the whole
// batch and the operator only needs one line to act on.
//
// Idempotent: a prefix with zero objects is not an error — the
// caller can re-run after a partial purge.
func (c *Client) RemoveAllUnderPrefix(ctx context.Context, prefix string) error {
	if prefix == "" {
		return errors.New("objectstore: empty prefix")
	}
	objects := c.c.ListObjects(ctx, c.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	})
	// Filter ListObjects errors INTO the channel surface the
	// caller pipes into RemoveObjects. minio-go's RemoveObjects
	// expects ObjectInfo values; the simplest adapter is a
	// goroutine that walks `objects` and re-emits, surfacing the
	// first list-side err separately.
	listErr := make(chan error, 1)
	feed := make(chan minio.ObjectInfo)
	go func() {
		defer close(feed)
		defer close(listErr)
		for obj := range objects {
			if obj.Err != nil {
				listErr <- obj.Err
				return
			}
			feed <- obj
		}
	}()
	for rmErr := range c.c.RemoveObjects(ctx, c.bucket, feed, minio.RemoveObjectsOptions{}) {
		if rmErr.Err != nil {
			return fmt.Errorf("objectstore: remove %q under %q: %w",
				rmErr.ObjectName, prefix, rmErr.Err)
		}
	}
	if err, ok := <-listErr; ok && err != nil {
		return fmt.Errorf("objectstore: list under %q: %w", prefix, err)
	}
	return nil
}

// Put writes body at key under the configured bucket. size = -1
// lets the SDK stream with unknown length (multipart under the
// hood); pass the exact byte count when known so MinIO can
// short-circuit to a single PUT.
//
// contentType is set on the object so a future browser preview of
// the dump renders inline ("text/html", "application/xml", ...)
// rather than triggering a download. Empty contentType makes the
// SDK fall back to application/octet-stream — acceptable but worse
// for an operator browsing the bucket; pass an explicit value when
// you know it.
func (c *Client) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	if key == "" {
		return errors.New("objectstore: empty key")
	}
	_, err := c.c.PutObject(ctx, c.bucket, key, body, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	if err != nil {
		return fmt.Errorf("objectstore: put %q: %w", key, err)
	}
	return nil
}

// Get opens the object at key for reading under the configured bucket.
// The returned ReadCloser is the caller's to close. minio's GetObject
// is lazy (a missing key surfaces on the first Read, not here), so the
// caller should wrap reads in an io.LimitReader and surface read errors;
// Stat is done up front so a missing object fails fast with a clear
// error rather than an opaque mid-stream one.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if key == "" {
		return nil, errors.New("objectstore: empty key")
	}
	obj, err := c.c.GetObject(ctx, c.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("objectstore: get %q: %w", key, err)
	}
	if _, err := obj.Stat(); err != nil {
		_ = obj.Close()
		return nil, fmt.Errorf("objectstore: stat %q: %w", key, err)
	}
	return obj, nil
}
