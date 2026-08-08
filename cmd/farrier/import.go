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

// runImport implements the `import` command (IMPT-001, IMPT-002): it calls
// Forgejo's migration API on the target instance to bring one repository in
// from GitHub or GitLab, optionally as a continuous mirror, and prints the
// same job event stream a dashboard would render over SSE.
func runImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	targetURL := fs.String("target", "", "base URL of the Farrier instance, e.g. https://git.example.com (required)")
	targetToken := fs.String("target-token", "", "API token for the target instance (required)")
	sourceURL := fs.String("source", "", "clone URL of the repository to import (required)")
	sourceToken := fs.String("source-token", "", "access token for the source forge (required)")
	service := fs.String("service", "", "source service: github or gitlab (default: autodetected from -source's host)")
	owner := fs.String("owner", "", "owner (user or organization) the repository lands under on the target instance (required)")
	name := fs.String("name", "", "repository name on the target instance (default: derived from -source)")
	private := fs.Bool("private", true, "mark the imported repository private on the target instance")
	mirror := fs.Bool("mirror", false, "keep the repository continuously synced from the source (IMPT-002)")
	mirrorInterval := fs.Duration("mirror-interval", 8*time.Hour, "how often to re-sync from the source; only used with -mirror")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	for flagName, value := range map[string]string{
		"target": *targetURL, "target-token": *targetToken,
		"source": *sourceURL, "source-token": *sourceToken,
		"owner": *owner,
	} {
		if strings.TrimSpace(value) == "" {
			fmt.Fprintf(os.Stderr, "farrier: import: -%s is required\n", flagName)
			return 2
		}
	}

	opts := importer.Options{
		TargetBaseURL: *targetURL,
		TargetToken:   keystore.NewSecret(*targetToken),
		SourceURL:     *sourceURL,
		SourceToken:   keystore.NewSecret(*sourceToken),
		Service:       *service,
		RepoOwner:     *owner,
		RepoName:      *name,
		Private:       *private,
		Mirror:        *mirror,
	}
	if *mirror {
		opts.MirrorInterval = *mirrorInterval
	}

	job := events.NewJob()
	sub, cancel := job.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		printEvents(sub)
		close(done)
	}()

	_, err := importer.Run(context.Background(), job, opts)
	<-done

	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: import: %v\n", err)
		return 1
	}
	return 0
}
