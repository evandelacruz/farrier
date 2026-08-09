package backup

import (
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
)

func TestReadManifestRoundTripsWriteManifest(t *testing.T) {
	dir := t.TempDir()
	want := &Manifest{
		ForgejoVersion:    "11.0.2",
		Timestamp:         time.Now().UTC().Truncate(time.Second),
		ChecksumAlgorithm: bundle.DefaultChecksumAlgorithm,
		Components: []Component{
			{Kind: bundle.StateKindKeys, Name: "secret_key", Path: "keys/secret_key", Checksum: "abc"},
		},
	}
	if err := writeManifest(dir, want); err != nil {
		t.Fatalf("writeManifest: %v", err)
	}

	got, err := ReadManifest(dir)
	if err != nil {
		t.Fatalf("ReadManifest: %v", err)
	}
	if got.ForgejoVersion != want.ForgejoVersion {
		t.Errorf("ForgejoVersion = %q, want %q", got.ForgejoVersion, want.ForgejoVersion)
	}
	if !got.Timestamp.Equal(want.Timestamp) {
		t.Errorf("Timestamp = %v, want %v", got.Timestamp, want.Timestamp)
	}
	if len(got.Components) != 1 || got.Components[0] != want.Components[0] {
		t.Errorf("Components = %+v, want %+v", got.Components, want.Components)
	}
}

func TestReadManifestMissingFile(t *testing.T) {
	if _, err := ReadManifest(t.TempDir()); err == nil {
		t.Fatal("ReadManifest: want error for a directory with no manifest, got nil")
	}
}
