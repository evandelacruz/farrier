package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/importer"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// runImport implements the `import` command (IMPT-001, IMPT-002, IMPT-003):
// it calls Forgejo's migration API on the target instance to bring one or
// more repositories in from GitHub or GitLab, optionally as a continuous
// mirror, and prints the same job event stream a dashboard would render
// over SSE. -source imports a single repository; -file imports a batch
// named in a YAML manifest, reporting each repository's own success or
// failure (IMPT-003).
//
// The target and source API tokens are read from FARRIER_TARGET_TOKEN and
// FARRIER_SOURCE_TOKEN in the process environment, never from a CLI flag —
// argv is visible to any local user via ps and lands in shell history, and
// `import` follows the same credentials-from-environment pattern `init`
// already uses for its ACME DNS-01 provider (tech-spec.md "Bundle
// creation"). A batch manifest carries the same restriction: it names
// repositories, never tokens.
func runImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	targetURL := fs.String("target", "", "base URL of the Farrier instance, e.g. https://git.example.com (required)")
	sourceURL := fs.String("source", "", "clone URL of the repository to import (required unless -file is set)")
	file := fs.String("file", "", "path to a YAML manifest listing repositories to import as a batch; mutually exclusive with -source")
	service := fs.String("service", "", "source service: github or gitlab (default: autodetected from -source's host); with -file, the default for entries that don't set their own")
	owner := fs.String("owner", "", "owner (user or organization) the repository lands under on the target instance (required unless every -file entry sets its own owner)")
	name := fs.String("name", "", "repository name on the target instance (default: derived from -source); ignored with -file")
	private := fs.Bool("private", true, "mark the imported repository private on the target instance; with -file, the default for entries that don't set their own")
	mirror := fs.Bool("mirror", false, "keep the repository continuously synced from the source; with -file, the default for entries that don't set their own")
	mirrorInterval := fs.Duration("mirror-interval", 8*time.Hour, "how often to re-sync from the source; only used with -mirror")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if strings.TrimSpace(*targetURL) == "" {
		fmt.Fprintln(os.Stderr, "farrier: import: -target is required")
		return 2
	}
	if (strings.TrimSpace(*sourceURL) == "") == (strings.TrimSpace(*file) == "") {
		fmt.Fprintln(os.Stderr, "farrier: import: exactly one of -source or -file is required")
		return 2
	}
	if strings.TrimSpace(*file) == "" && strings.TrimSpace(*owner) == "" {
		fmt.Fprintln(os.Stderr, "farrier: import: -owner is required")
		return 2
	}

	targetToken := os.Getenv("FARRIER_TARGET_TOKEN")
	sourceToken := os.Getenv("FARRIER_SOURCE_TOKEN")
	for envName, value := range map[string]string{
		"FARRIER_TARGET_TOKEN": targetToken, "FARRIER_SOURCE_TOKEN": sourceToken,
	} {
		if strings.TrimSpace(value) == "" {
			fmt.Fprintf(os.Stderr, "farrier: import: %s must be set in the environment\n", envName)
			return 2
		}
	}

	defaults := importer.Options{
		TargetBaseURL: *targetURL,
		TargetToken:   keystore.NewSecret(targetToken),
		SourceToken:   keystore.NewSecret(sourceToken),
		Service:       *service,
		RepoOwner:     *owner,
		Private:       *private,
		Mirror:        *mirror,
	}
	if *mirror {
		defaults.MirrorInterval = *mirrorInterval
	}

	// Resolve everything that can fail without a job — reading and
	// validating a batch manifest — before starting one: a job that never
	// runs never emits its terminal event, and runJob below would then
	// block forever waiting for a stream that never closes.
	var repos []importer.Options
	if *file != "" {
		manifestRepos, err := loadManifest(*file, defaults)
		if err != nil {
			fmt.Fprintf(os.Stderr, "farrier: import: %v\n", err)
			return 2
		}
		repos = manifestRepos
	}

	job := events.NewJob()
	err := runJob(job, func() error {
		if *file != "" {
			_, runErr := importer.RunBatch(context.Background(), job, importer.BatchOptions{Repos: repos})
			return runErr
		}
		opts := defaults
		opts.SourceURL = *sourceURL
		opts.RepoName = *name
		_, runErr := importer.Run(context.Background(), job, opts)
		return runErr
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: import: %v\n", err)
		return 1
	}
	return 0
}

// loadManifest reads and resolves a batch import manifest into one Options
// per repository, layered over defaults (IMPT-003).
func loadManifest(path string, defaults importer.Options) ([]importer.Options, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	manifest, err := importer.ParseManifest(data)
	if err != nil {
		return nil, err
	}
	return manifest.RepoOptions(defaults)
}
