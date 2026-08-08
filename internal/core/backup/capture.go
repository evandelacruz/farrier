package backup

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/state"
)

// Directory and file names within a snapshot (tech-spec.md "Snapshot
// format").
const (
	databaseFile = "db.sqlite"
	reposDir     = "repos"
	blobsDir     = "blobs"
	keysDir      = "keys"
)

// captureDatabase writes exporter's snapshot to dir/db.sqlite and returns
// its Component entry.
func captureDatabase(ctx context.Context, dir string, exporter state.DatabaseExporter) (Component, error) {
	rc, err := exporter.Snapshot(ctx)
	if err != nil {
		return Component{}, fmt.Errorf("backup: capture database: %w", err)
	}
	defer rc.Close()

	checksum, err := writeChecksummed(filepath.Join(dir, databaseFile), rc)
	if err != nil {
		return Component{}, fmt.Errorf("backup: capture database: %w", err)
	}
	return Component{Kind: bundle.StateKindDatabase, Name: databaseFile, Path: databaseFile, Checksum: checksum}, nil
}

// captureGitRefs records every remote's ref state — HEAD, packed-refs,
// refs/ — through capturer.Refs, writing each to
// dir/repos/<name>.refs.tar. Run calls this while params.PushHold is
// engaged (BKUP-002): it's the only part of git capture that has to happen
// inside the hold, since it's the only part a push actually changes.
func captureGitRefs(ctx context.Context, dir string, capturer GitCapturer, remotes []state.Remote) ([]Component, error) {
	components := make([]Component, 0, len(remotes))
	for _, remote := range remotes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		component, err := captureOneRepoRefs(ctx, dir, capturer, remote)
		if err != nil {
			return nil, err
		}
		components = append(components, component)
	}
	return components, nil
}

// captureGitObjects archives every remote's full object store through
// capturer, and writes it to dir/repos/<name>.tar, returning one Component
// per repository in remotes order. Run calls this only after params.PushHold
// has released (BKUP-002): git objects are immutable and append-only, so a
// push landing while this runs can only add objects, never disturb a ref
// captureGitRefs already pinned during the hold.
func captureGitObjects(ctx context.Context, dir string, capturer GitCapturer, remotes []state.Remote) ([]Component, error) {
	components := make([]Component, 0, len(remotes))
	for _, remote := range remotes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		component, err := captureOneRepo(ctx, dir, capturer, remote)
		if err != nil {
			return nil, err
		}
		components = append(components, component)
	}
	return components, nil
}

func captureOneRepoRefs(ctx context.Context, dir string, capturer GitCapturer, remote state.Remote) (Component, error) {
	rc, err := capturer.Refs(ctx, remote)
	if err != nil {
		return Component{}, fmt.Errorf("backup: capture git: refs %s: %w", remote.Name, err)
	}
	defer rc.Close()

	relPath := path.Join(reposDir, remote.Name+".refs.tar")
	checksum, err := writeChecksummed(filepath.Join(dir, filepath.FromSlash(relPath)), rc)
	if err != nil {
		return Component{}, fmt.Errorf("backup: capture git: refs %s: %w", remote.Name, err)
	}
	return Component{Kind: bundle.StateKindGit, Name: remote.Name, Path: relPath, Checksum: checksum}, nil
}

func captureOneRepo(ctx context.Context, dir string, capturer GitCapturer, remote state.Remote) (Component, error) {
	rc, err := capturer.Archive(ctx, remote)
	if err != nil {
		return Component{}, fmt.Errorf("backup: capture git: archive %s: %w", remote.Name, err)
	}
	defer rc.Close()

	relPath := path.Join(reposDir, remote.Name+".tar")
	checksum, err := writeChecksummed(filepath.Join(dir, filepath.FromSlash(relPath)), rc)
	if err != nil {
		return Component{}, fmt.Errorf("backup: capture git: archive %s: %w", remote.Name, err)
	}
	return Component{Kind: bundle.StateKindGit, Name: remote.Name, Path: relPath, Checksum: checksum}, nil
}

// captureBlobs lists every object blobExporter exposes and writes each to
// dir/blobs/<key>, returning one Component per object sorted by key —
// blob.Adapter.List makes no ordering guarantee, so captureBlobs imposes one
// to keep the manifest's component order reproducible across runs.
func captureBlobs(ctx context.Context, dir string, blobExporter state.BlobExporter) ([]Component, error) {
	objects, err := blobExporter.List(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("backup: capture blobs: list: %w", err)
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })

	components := make([]Component, 0, len(objects))
	for _, obj := range objects {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		component, err := captureOneBlob(ctx, dir, blobExporter, obj.Key)
		if err != nil {
			return nil, err
		}
		components = append(components, component)
	}
	return components, nil
}

func captureOneBlob(ctx context.Context, dir string, blobExporter state.BlobExporter, key string) (Component, error) {
	rc, err := blobExporter.Get(ctx, key)
	if err != nil {
		return Component{}, fmt.Errorf("backup: capture blobs: get %s: %w", key, err)
	}
	defer rc.Close()

	relPath := path.Join(blobsDir, key)
	checksum, err := writeChecksummed(filepath.Join(dir, filepath.FromSlash(relPath)), rc)
	if err != nil {
		return Component{}, fmt.Errorf("backup: capture blobs: get %s: %w", key, err)
	}
	return Component{Kind: bundle.StateKindBlobs, Name: key, Path: relPath, Checksum: checksum}, nil
}

// captureKeys resolves every name keyExporter enumerates and writes each to
// dir/keys/<name>, returning one Component per key in Names order. Secret
// values only ever flow into the file writeChecksummed creates — never into
// an error message, a log line, or an event Detail (KEY-003).
func captureKeys(ctx context.Context, dir string, keyExporter state.KeyExporter) ([]Component, error) {
	names := keyExporter.Names()
	components := make([]Component, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		component, err := captureOneKey(ctx, dir, keyExporter, name)
		if err != nil {
			return nil, err
		}
		components = append(components, component)
	}
	return components, nil
}

func captureOneKey(ctx context.Context, dir string, keyExporter state.KeyExporter, name string) (Component, error) {
	secret, err := keyExporter.Resolve(ctx, name)
	if err != nil {
		return Component{}, fmt.Errorf("backup: capture keys: resolve %s: %w", name, err)
	}

	relPath := path.Join(keysDir, name)
	checksum, err := writeChecksummed(filepath.Join(dir, filepath.FromSlash(relPath)), strings.NewReader(secret.Reveal()))
	if err != nil {
		return Component{}, fmt.Errorf("backup: capture keys: resolve %s: %w", name, err)
	}
	return Component{Kind: bundle.StateKindKeys, Name: name, Path: relPath, Checksum: checksum}, nil
}
