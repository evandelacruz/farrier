// verify.go implements BKUP-004: verifying a snapshot at creation, so a
// backup that fails verification fails loudly at backup time rather than at
// the moment an operator needs it (spec.md "Verification"). tech-spec.md
// "What the system owns: verified restores" defines the same three checks
// for restore (RSTR-003): manifest completeness, checksums, and
// cross-consistency. Verify implements all three against the plain
// snapshot directory backup.Run produces, so restore can reuse it unchanged
// once it exists (tech-spec.md "Snapshot format": "Verification at
// creation and at restore runs the same code path").
package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"

	_ "modernc.org/sqlite"
)

// Defect is one specific way a snapshot failed verification: which check
// found it (completeness, checksum, or cross-consistency), which component
// or database reference it's about, and what's wrong. Verify collects every
// Defect it finds rather than stopping at the first one, so a caller — the
// backup CLI eventually, Run today — can report exactly what's broken in
// one pass instead of a generic "verification failed" (BKUP-004).
type Defect struct {
	Check   string
	Subject string
	Detail  string
}

func (d Defect) String() string {
	if d.Subject == "" {
		return fmt.Sprintf("%s: %s", d.Check, d.Detail)
	}
	return fmt.Sprintf("%s: %s: %s", d.Check, d.Subject, d.Detail)
}

// VerifyError reports every Defect Verify found. Its presence — as opposed
// to a nil error — is what "fails verification" means: Verify never
// returns a partial pass.
type VerifyError struct {
	Defects []Defect
}

func (e *VerifyError) Error() string {
	lines := make([]string, len(e.Defects))
	for i, d := range e.Defects {
		lines[i] = d.String()
	}
	return fmt.Sprintf("%d defect(s) found: %s", len(e.Defects), strings.Join(lines, "; "))
}

// Verify checks the snapshot at dir against manifest:
//
//   - Completeness: the manifest declares a checksum algorithm Verify knows
//     how to check, exactly one database component, and every name in
//     keyNames captured as a key component.
//   - Checksums: every component's file on disk, recomputed, matches the
//     checksum manifest recorded for it at capture time.
//   - Cross-consistency: the captured database's own repository, LFS, and
//     attachment references each resolve to a component this same snapshot
//     captured (tech-spec.md "What the system owns: verified restores").
//
// keyNames is the bundle's full set of expected key material names
// (state.KeyExporter.Names()) — Run passes params.Keys.Names() directly so
// Verify itself stays decoupled from the state package, the same shape
// capture.go's helpers already take their inputs in.
//
// It returns a *VerifyError naming every defect found, or nil if the
// snapshot is clean.
func Verify(ctx context.Context, dir string, manifest *Manifest, keyNames []string) error {
	var defects []Defect
	defects = append(defects, verifyCompleteness(manifest, keyNames)...)
	defects = append(defects, verifyChecksums(dir, manifest)...)
	defects = append(defects, verifyCrossConsistency(ctx, dir, manifest)...)

	if len(defects) == 0 {
		return nil
	}
	return &VerifyError{Defects: defects}
}

// verifyCompleteness checks the manifest's own structure: a checksum
// algorithm Verify can act on, exactly one database component, and every
// name in keyNames present among the manifest's key components.
func verifyCompleteness(manifest *Manifest, keyNames []string) []Defect {
	var defects []Defect

	if manifest.ChecksumAlgorithm != bundle.DefaultChecksumAlgorithm {
		defects = append(defects, Defect{
			Check:  "completeness",
			Detail: fmt.Sprintf("manifest checksum algorithm is %q, want %q", manifest.ChecksumAlgorithm, bundle.DefaultChecksumAlgorithm),
		})
	}

	dbCount := 0
	haveKey := make(map[string]bool, len(manifest.Components))
	for _, c := range manifest.Components {
		switch c.Kind {
		case bundle.StateKindDatabase:
			dbCount++
		case bundle.StateKindKeys:
			haveKey[c.Name] = true
		}
	}
	if dbCount != 1 {
		defects = append(defects, Defect{
			Check:  "completeness",
			Detail: fmt.Sprintf("manifest has %d database component(s), want exactly 1", dbCount),
		})
	}
	for _, name := range keyNames {
		if !haveKey[name] {
			defects = append(defects, Defect{
				Check:   "completeness",
				Subject: name,
				Detail:  "key material missing from snapshot manifest",
			})
		}
	}
	return defects
}

// verifyChecksums recomputes every component's checksum from the file dir
// holds for it and compares it against what the manifest recorded at
// capture time. A file that can't be opened or read is reported the same
// way as a mismatch: either one means the snapshot doesn't hold what the
// manifest says it does.
func verifyChecksums(dir string, manifest *Manifest) []Defect {
	var defects []Defect
	for _, c := range manifest.Components {
		full := filepath.Join(dir, filepath.FromSlash(c.Path))
		got, err := checksumFile(full)
		if err != nil {
			defects = append(defects, Defect{
				Check:   "checksum",
				Subject: c.Path,
				Detail:  fmt.Sprintf("read component: %s", err),
			})
			continue
		}
		if got != c.Checksum {
			defects = append(defects, Defect{
				Check:   "checksum",
				Subject: c.Path,
				Detail:  fmt.Sprintf("checksum mismatch: manifest says %s, computed %s", c.Checksum, got),
			})
		}
	}
	return defects
}

func checksumFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyCrossConsistency opens the snapshot's own captured database
// (dir/db.sqlite) and checks its repository, LFS, and attachment
// references each resolve to a component this same snapshot captured. A
// database that can't be opened at all — corrupt, truncated, not a SQLite
// file — is itself a defect rather than a hard error: Verify's contract is
// to name what's wrong, and "the captured database doesn't open" is exactly
// the kind of thing a backup that fails verification is supposed to catch.
func verifyCrossConsistency(ctx context.Context, dir string, manifest *Manifest) []Defect {
	dbPath := filepath.Join(dir, databaseFile)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return []Defect{{Check: "cross-consistency", Subject: databaseFile, Detail: fmt.Sprintf("open captured database: %s", err)}}
	}
	defer db.Close()
	// database/sql pools lazily and only actually opens the file on first
	// use, so confirm it's really readable here rather than deferring that
	// discovery to the first query below.
	if err := db.PingContext(ctx); err != nil {
		return []Defect{{Check: "cross-consistency", Subject: databaseFile, Detail: fmt.Sprintf("open captured database: %s", err)}}
	}

	var defects []Defect
	defects = append(defects, verifyRepositoryReferences(ctx, db, manifest)...)
	defects = append(defects, verifyBlobReferences(ctx, db, "lfs_meta_object", "oid", manifest)...)
	defects = append(defects, verifyBlobReferences(ctx, db, "attachment", "uuid", manifest)...)
	return defects
}

// verifyRepositoryReferences checks that every row in Forgejo's repository
// table (owner_name, lower_name — the same denormalized pair Forgejo's own
// repository path, "<owner>/<repo>.git", is built from) has a matching git
// component of each kind captureGitRefs and captureGitObjects record for
// every repository: "<owner>/<repo>.refs.tar" and "<owner>/<repo>.tar".
func verifyRepositoryReferences(ctx context.Context, db *sql.DB, manifest *Manifest) []Defect {
	haveRefs := make(map[string]bool)
	haveObjects := make(map[string]bool)
	for _, c := range manifest.Components {
		if c.Kind != bundle.StateKindGit {
			continue
		}
		switch {
		case strings.HasSuffix(c.Path, ".refs.tar"):
			haveRefs[c.Name] = true
		case strings.HasSuffix(c.Path, ".tar"):
			haveObjects[c.Name] = true
		}
	}

	rows, err := db.QueryContext(ctx, `SELECT owner_name, lower_name FROM repository`)
	if err != nil {
		return []Defect{{Check: "cross-consistency", Subject: "repository", Detail: fmt.Sprintf("query repository table: %s", err)}}
	}
	defer rows.Close()

	var defects []Defect
	for rows.Next() {
		var owner, name string
		if err := rows.Scan(&owner, &name); err != nil {
			defects = append(defects, Defect{Check: "cross-consistency", Subject: "repository", Detail: fmt.Sprintf("scan row: %s", err)})
			continue
		}
		full := owner + "/" + name
		if !haveObjects[full] {
			defects = append(defects, Defect{Check: "cross-consistency", Subject: full, Detail: "repository row has no matching captured object archive"})
		}
		if !haveRefs[full] {
			defects = append(defects, Defect{Check: "cross-consistency", Subject: full, Detail: "repository row has no matching captured ref state"})
		}
	}
	if err := rows.Err(); err != nil {
		defects = append(defects, Defect{Check: "cross-consistency", Subject: "repository", Detail: fmt.Sprintf("iterate rows: %s", err)})
	}
	return defects
}

// verifyBlobReferences checks that every value of column in table — an LFS
// oid or an attachment uuid, the identifier Forgejo embeds somewhere in the
// object's storage key regardless of the exact path convention its storage
// backend uses — appears as a substring of at least one captured blob
// component's name. Matching on containment rather than an exact key keeps
// this decoupled from the nesting/prefix scheme, which is a property of
// whichever blob.Adapter the operator configured (BLOB-001, BLOB-002), not
// of Forgejo's database.
//
// table and column are always one of the two fixed pairs verifyCrossConsistency
// passes in — never caller input — so building the query with fmt.Sprintf
// carries no injection risk (the same reasoning forge.resetRunning
// documents for its own fixed table name).
//
// CI artifacts and avatars aren't cross-checked yet: their schema and
// storage-key convention aren't established anywhere in this codebase, and
// guessing at Forgejo internals this codebase doesn't already depend on
// risks a check that's confidently wrong rather than usefully strict. This
// is a scope limitation to flag, not a silent gap: repositories and LFS/
// attachment blobs are checked; CI artifacts and avatars are not.
func verifyBlobReferences(ctx context.Context, db *sql.DB, table, column string, manifest *Manifest) []Defect {
	var blobNames []string
	for _, c := range manifest.Components {
		if c.Kind == bundle.StateKindBlobs {
			blobNames = append(blobNames, c.Name)
		}
	}

	rows, err := db.QueryContext(ctx, fmt.Sprintf("SELECT %s FROM %s", column, table))
	if err != nil {
		return []Defect{{Check: "cross-consistency", Subject: table, Detail: fmt.Sprintf("query %s table: %s", table, err)}}
	}
	defer rows.Close()

	var defects []Defect
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			defects = append(defects, Defect{Check: "cross-consistency", Subject: table, Detail: fmt.Sprintf("scan row: %s", err)})
			continue
		}
		if id == "" {
			continue
		}
		if !anyContains(blobNames, id) {
			defects = append(defects, Defect{
				Check:   "cross-consistency",
				Subject: fmt.Sprintf("%s.%s=%s", table, column, id),
				Detail:  "no captured blob matches this reference",
			})
		}
	}
	if err := rows.Err(); err != nil {
		defects = append(defects, Defect{Check: "cross-consistency", Subject: table, Detail: fmt.Sprintf("iterate rows: %s", err)})
	}
	return defects
}

func anyContains(names []string, substr string) bool {
	for _, n := range names {
		if strings.Contains(n, substr) {
			return true
		}
	}
	return false
}
