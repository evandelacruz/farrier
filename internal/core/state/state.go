// Package state defines the export interface for each of the four kinds of
// state a bundle carries (spec.md "The four kinds of state"): git data,
// database, blobs, and key material. Each kind gets its own interface here
// because each has a different natural export mechanism; backup (BKUP-001)
// and the operator's own replication tooling (spec.md "What the operator
// owns") both consume these interfaces rather than reaching into Forgejo's
// storage directly.
//
// Git (STATE-001) is the first kind implemented; database, blobs, and key
// material follow under their own requirement IDs.
package state

// Remote is one repository exposed as a mirrorable git remote: Name is its
// slash-separated "owner/repo" identity, URL is an address a git client can
// plug straight into `git remote add --mirror` and `git push --mirror` —
// either a local filesystem path or an ssh:// URL, depending on the
// GitExporter that produced it.
type Remote struct {
	Name string
	URL  string
}
