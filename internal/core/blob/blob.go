// Package blob implements the storage-adapter abstraction blobs — LFS
// objects, CI artifacts, avatars — are accessed through (spec.md "Blobs").
//
// Adapter is the published Go interface every adapter satisfies. Two ship
// in-tree: local (BLOB-001), backed by a filesystem path, and s3
// (BLOB-002), backed by any S3-compatible endpoint. ExecAdapter satisfies
// it too, reaching a third-party adapter through the CORE-003 exec
// protocol instead of an in-tree implementation.
package blob

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrNotFound is returned by Get when key does not exist in the adapter's
// backing store.
var ErrNotFound = errors.New("blob: not found")

// Object describes one stored object: its key, size in bytes, and when it
// was last written.
//
// Modified is what STAT-002's replication-lag reporting is built on: the
// newest object's Modified time across a destination is the last backup,
// now minus that is the lag. local and s3 always populate it (from the
// filesystem's mtime and the endpoint's Last-Modified, respectively); a
// third-party exec adapter (CORE-003) populates it by returning a
// "modified" field from its "list" method, and an older one that predates
// the field simply omits it, which decodes as the zero value — callers
// must treat a zero Modified as "unknown", never as "very old".
type Object struct {
	Key      string
	Size     int64
	Modified time.Time
}

// Adapter is the storage-adapter abstraction: List, Get, and Put all
// stream, so a caller never has to hold a whole LFS object or CI artifact
// in memory to move it.
type Adapter interface {
	// List returns every object whose key has the given prefix, in no
	// particular order. An empty prefix lists everything.
	List(ctx context.Context, prefix string) ([]Object, error)

	// Get opens a stream for the object at key. The caller must Close the
	// returned reader. Get returns an error satisfying errors.Is(err,
	// ErrNotFound) if key does not exist.
	Get(ctx context.Context, key string) (io.ReadCloser, error)

	// Put streams r to the object at key, creating it or replacing it
	// whole. size is the exact number of bytes r will yield; pass -1 if
	// the size isn't known ahead of the stream.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
}
