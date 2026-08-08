// Package backup — this file: resolving `backup --to <uri>` into a
// blob.Adapter (BKUP-005), and the snapshot naming convention Write uses
// once it has one.
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/blob"
)

// snapshotTimeFormat renders a snapshot's capture time so object keys sort
// correctly both lexicographically and chronologically, and stay safe as
// both an S3 key and a filesystem name.
const snapshotTimeFormat = "20060102T150405Z"

// snapshotSuffix marks a destination object as an age-encrypted snapshot
// archive (tech-spec.md "Snapshot encryption").
const snapshotSuffix = ".age"

// SnapshotKey returns the object key Write stores the snapshot captured at t
// under. This is the stable naming convention tech-spec.md "Status" defers
// to BKUP-005: status's future last-backup-age lookup (STAT-001) finds the
// newest snapshot the same way replication lag already does (STAT-002) — by
// listing a destination and taking the newest Modified time — so the
// convention only has to keep every snapshot object sorted and
// unambiguously identifiable, not carry meaning status has to parse back
// out of the name.
func SnapshotKey(t time.Time) string {
	return t.UTC().Format(snapshotTimeFormat) + snapshotSuffix
}

// OpenDestination resolves the golden path's `backup --to <uri>` (spec.md
// "Golden path") into a blob.Adapter: a value starting with "s3://" selects
// the s3 adapter (BLOB-002); anything else is a filesystem directory path
// and selects the local adapter (BLOB-001) — together, "an S3-compatible
// URI or a filesystem path" (BKUP-005). This resolution is backup's own
// concern, not a general blob-package entry point: status will reuse it
// once it locates the operator's configured destination itself
// (tech-spec.md "Status", STAT-002).
//
// An "s3://" URI has the shape
// s3://<bucket>[/<key-prefix>]?endpoint=<host[:port]>[&region=<region>][&pathStyle=true][&ssl=false].
// endpoint is required — there is no single default that covers every
// S3-compatible service. Credentials are read from the AWS_ACCESS_KEY_ID
// and AWS_SECRET_ACCESS_KEY environment variables, the names virtually
// every S3-compatible tool already honors, never from the URI itself or a
// CLI flag — so a destination's secret access key never appears in `ps`
// output or shell history, the same posture IMPT-001's source and target
// tokens take (tech-spec.md "Importing repositories").
func OpenDestination(uri string) (blob.Adapter, error) {
	if strings.TrimSpace(uri) == "" {
		return nil, errors.New("backup: destination: uri is required")
	}
	if !strings.HasPrefix(uri, "s3://") {
		return blob.NewLocal(uri)
	}
	return openS3Destination(uri)
}

func openS3Destination(uri string) (blob.Adapter, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return nil, fmt.Errorf("backup: destination: parse %q: %w", uri, err)
	}
	bucket := u.Host
	if bucket == "" {
		return nil, fmt.Errorf("backup: destination: %q: bucket is required (s3://<bucket>/...)", uri)
	}
	prefix := strings.Trim(u.Path, "/")

	query := u.Query()
	endpoint := query.Get("endpoint")
	if endpoint == "" {
		return nil, fmt.Errorf("backup: destination: %q: endpoint query parameter is required", uri)
	}

	useSSL := true
	if v := query.Get("ssl"); v != "" {
		useSSL, err = strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("backup: destination: %q: ssl: %w", uri, err)
		}
	}
	var pathStyle bool
	if v := query.Get("pathStyle"); v != "" {
		pathStyle, err = strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("backup: destination: %q: pathStyle: %w", uri, err)
		}
	}

	accessKeyID := os.Getenv("AWS_ACCESS_KEY_ID")
	secretAccessKey := os.Getenv("AWS_SECRET_ACCESS_KEY")
	if accessKeyID == "" || secretAccessKey == "" {
		return nil, errors.New("backup: destination: AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY must be set for an s3:// destination")
	}

	adapter, err := blob.NewS3(blob.S3Config{
		Endpoint:        endpoint,
		Region:          query.Get("region"),
		Bucket:          bucket,
		AccessKeyID:     accessKeyID,
		SecretAccessKey: secretAccessKey,
		UseSSL:          useSSL,
		PathStyle:       pathStyle,
	})
	if err != nil {
		return nil, fmt.Errorf("backup: destination: %w", err)
	}
	if prefix == "" {
		return adapter, nil
	}
	return &prefixedAdapter{inner: adapter, prefix: prefix + "/"}, nil
}

// prefixedAdapter scopes an underlying blob.Adapter to keys under a fixed
// prefix, so several bundles can share one bucket (an s3:// destination
// URI's optional path segment) the same way a filesystem destination scopes
// to a directory. List strips the prefix back off returned keys so a
// prefixedAdapter is indistinguishable from an adapter dedicated to that
// prefix alone — which is what every other caller, including status's
// future replication-lag and last-backup-age lookups, needs it to be.
type prefixedAdapter struct {
	inner  blob.Adapter
	prefix string
}

func (a *prefixedAdapter) List(ctx context.Context, prefix string) ([]blob.Object, error) {
	objects, err := a.inner.List(ctx, a.prefix+prefix)
	if err != nil {
		return nil, err
	}
	out := make([]blob.Object, len(objects))
	for i, o := range objects {
		o.Key = strings.TrimPrefix(o.Key, a.prefix)
		out[i] = o
	}
	return out, nil
}

func (a *prefixedAdapter) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return a.inner.Get(ctx, a.prefix+key)
}

func (a *prefixedAdapter) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	return a.inner.Put(ctx, a.prefix+key, r, size)
}
