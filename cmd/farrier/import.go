package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/importrepo"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// runImport implements the `import` command (IMPT-001): it migrates one
// repository from GitHub or GitLab into the bundle's forge — code, full
// history, LFS objects, default branch — by calling Forgejo's own
// migration API at the bundle's domain (UP-002 puts the forge there).
func runImport(args []string) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	bundleDir := fs.String("bundle", "", "path to the bundle directory (required)")
	forgeToken := fs.String("forge-token", "", "Forgejo API token authorized to create repositories (required)")
	source := fs.String("source", "", "source repository clone URL, e.g. https://github.com/owner/repo.git (required)")
	sourceToken := fs.String("source-token", "", "access token for the source GitHub/GitLab repository (required)")
	service := fs.String("service", "", "source forge: github or gitlab (default: inferred from -source)")
	owner := fs.String("owner", "", "Forgejo user or organization to migrate into (default: the forge-token's own account)")
	repoName := fs.String("repo-name", "", "migrated repository name (default: derived from -source)")
	private := fs.Bool("private", true, "migrate the repository as private")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if strings.TrimSpace(*bundleDir) == "" {
		fmt.Fprintln(os.Stderr, "farrier: import: -bundle is required")
		return 2
	}
	if strings.TrimSpace(*forgeToken) == "" {
		fmt.Fprintln(os.Stderr, "farrier: import: -forge-token is required")
		return 2
	}
	if strings.TrimSpace(*source) == "" {
		fmt.Fprintln(os.Stderr, "farrier: import: -source is required")
		return 2
	}
	if strings.TrimSpace(*sourceToken) == "" {
		fmt.Fprintln(os.Stderr, "farrier: import: -source-token is required")
		return 2
	}

	b, err := bundle.Load(*bundleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: import: load bundle: %v\n", err)
		return 1
	}

	job := events.NewJob()
	sub, cancel := job.Subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		printEvents(sub)
		close(done)
	}()

	_, importErr := importrepo.Run(context.Background(), job, importrepo.Options{
		API:         "https://" + b.Manifest.Domain,
		AdminToken:  keystore.NewSecret(*forgeToken),
		SourceURL:   *source,
		SourceToken: keystore.NewSecret(*sourceToken),
		Service:     *service,
		Owner:       *owner,
		RepoName:    *repoName,
		Private:     *private,
	})
	<-done

	if importErr != nil {
		fmt.Fprintf(os.Stderr, "farrier: import: %v\n", importErr)
		return 1
	}
	return 0
}
