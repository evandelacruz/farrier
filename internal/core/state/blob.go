package state

import (
	"context"
	"io"

	"github.com/evandelacruz/farrier/internal/core/blob"
)

// BlobExporter exposes a forge's blob storage — LFS objects, CI artifacts,
// avatars (STATE-003, spec.md "Blobs") — for capture into a snapshot's
// blobs/ directory (tech-spec.md "Snapshot format"). It is exactly the read
// side of blob.Adapter (BLOB-001, BLOB-002): List enumerates every object
// under a prefix, Get streams one open. Blob export is not a separate
// mechanism from ordinary blob access — every adapter, in-tree or
// third-party (CORE-003), already satisfies BlobExporter with zero extra
// code, so backup (BKUP-001) and the operator's own replication tooling
// walk the same objects through the same interface.
type BlobExporter interface {
	// List returns every object whose key has the given prefix, in no
	// particular order. An empty prefix lists everything.
	List(ctx context.Context, prefix string) ([]blob.Object, error)

	// Get opens a stream for the object at key. The caller must Close the
	// returned reader.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
}

// blob.Adapter's method set is a superset of BlobExporter's, so every
// shipped adapter already exports blobs without any adapting code.
var (
	_ BlobExporter = (*blob.LocalAdapter)(nil)
	_ BlobExporter = (*blob.S3Adapter)(nil)
	_ BlobExporter = (*blob.ExecAdapter)(nil)
)
