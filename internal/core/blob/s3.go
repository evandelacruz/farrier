package blob

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config configures the "s3" blob adapter (BLOB-002) against any
// S3-compatible endpoint — AWS S3 itself or any service that implements its
// API (MinIO, and most self-hosted object stores).
//
// Every field here is non-secret bundle driver config (bundle.DriverRef)
// except SecretAccessKey, which a caller resolves through a keystore driver
// before building S3Config — this package never reads or writes the bundle
// directory.
type S3Config struct {
	// Endpoint is host[:port], with no scheme.
	Endpoint string
	Region   string
	Bucket   string

	AccessKeyID     string
	SecretAccessKey string

	// UseSSL selects https vs http for Endpoint.
	UseSSL bool
	// PathStyle forces path-style bucket addressing
	// (endpoint/bucket/key instead of bucket.endpoint/key), required by
	// most non-AWS S3-compatible services.
	PathStyle bool
}

// S3Adapter is the "s3" blob adapter: List, Get, and Put against a single
// bucket on an S3-compatible endpoint.
type S3Adapter struct {
	client *minio.Client
	bucket string
}

// NewS3 builds an S3Adapter from cfg.
func NewS3(cfg S3Config) (*S3Adapter, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("blob: s3: endpoint is required")
	}
	if cfg.Bucket == "" {
		return nil, errors.New("blob: s3: bucket is required")
	}

	lookup := minio.BucketLookupAuto
	if cfg.PathStyle {
		lookup = minio.BucketLookupPath
	}
	region := cfg.Region
	if region == "" {
		// Many S3-compatible services don't implement GetBucketLocation;
		// a fixed region skips that lookup instead of failing against
		// them. AWS itself accepts us-east-1 as a request region
		// regardless of a bucket's actual region.
		region = "us-east-1"
	}

	client, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		Secure:       cfg.UseSSL,
		Region:       region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, fmt.Errorf("blob: s3: %w", err)
	}
	return &S3Adapter{client: client, bucket: cfg.Bucket}, nil
}

// List returns every object in the bucket whose key has the given prefix.
func (a *S3Adapter) List(ctx context.Context, prefix string) ([]Object, error) {
	var objects []Object
	for info := range a.client.ListObjects(ctx, a.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if info.Err != nil {
			return nil, fmt.Errorf("blob: s3: list: %w", info.Err)
		}
		objects = append(objects, Object{Key: info.Key, Size: info.Size, Modified: info.LastModified})
	}
	return objects, nil
}

// Get opens a stream for the object at key.
func (a *S3Adapter) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := a.client.GetObject(ctx, a.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("blob: s3: get %s: %w", key, err)
	}
	// minio's GetObject doesn't hit the network until the first read or
	// Stat, so a missing key surfaces here rather than from GetObject
	// itself.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, fmt.Errorf("blob: s3: get %s: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("blob: s3: get %s: %w", key, err)
	}
	return obj, nil
}

// Put streams r to the object at key, creating it or replacing it whole.
// size may be -1 if the stream's length isn't known ahead of time.
func (a *S3Adapter) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	if _, err := a.client.PutObject(ctx, a.bucket, key, r, size, minio.PutObjectOptions{}); err != nil {
		return fmt.Errorf("blob: s3: put %s: %w", key, err)
	}
	return nil
}
