package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/publish"
)

// runPublish implements the `publish` command (IMPT-004): it creates the
// project's repository on its own instance, pushes the folder's existing
// history, and sets `origin` to the instance's git-over-SSH URL, printing
// the same job event stream a dashboard would render over SSE.
//
// Every default points at the project the operator is standing in: the
// bundle at .farrier/, the repository named for the folder, the instance
// addressed by the bundle's own domain, and the repository private. The
// intended invocation is `cd my-project && farrier publish`.
//
// A nameless instance has no domain to be addressed by, so it is reached at
// the address it was deployed to: `-address`, or by default the host of
// `-target`, which makes `farrier publish -target http://192.168.1.5:8222`
// work as typed.
//
// The account the token belongs to must have an SSH public key registered
// before it can be pushed to, and a fresh instance has none. So when it has
// none and the operator named no -ssh-key, publish registers the operator's
// own public key and says which file it took — that is what makes the
// README's `farrier publish` with no flags work on a new instance.
//
// The instance API token is read from FARRIER_TARGET_TOKEN in the process
// environment, never from a flag, for the reason `import` reads its tokens
// there: argv is visible to any local user via ps and lands in shell
// history.
func runPublish(args []string) int {
	fs := flag.NewFlagSet("publish", flag.ContinueOnError)
	dir := fs.String("dir", ".", "project folder to publish")
	bundleDir := fs.String("bundle", "", "path to the bundle directory (default: .farrier inside -dir)")
	targetURL := fs.String("target", "", "base URL of the instance's API (default: the bundle's own public URL — its domain over HTTPS, at the port clients connect on)")
	address := fs.String("address", "", "address a nameless instance is reached at, for the git remote and the host key check (default: the host of -target); a named instance is reached at its domain and takes no address")
	owner := fs.String("owner", "", "owner (user or organization) the repository lands under on the instance (default: the account the token belongs to)")
	name := fs.String("name", "", "repository name on the instance (default: the project folder's name)")
	private := fs.Bool("private", true, "create the repository private")
	remote := fs.String("remote", publish.DefaultRemoteName, "git remote to point at the instance")
	sshKey := fs.String("ssh-key", "", "path to the SSH public key to register with the instance account, so pushes from this machine are authorized (default: ~/.ssh/id_ed25519.pub, then ~/.ssh/id_rsa.pub, registered only when the account has no key yet)")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	token := os.Getenv("FARRIER_TARGET_TOKEN")
	if strings.TrimSpace(token) == "" {
		fmt.Fprintln(os.Stderr, "farrier: publish: FARRIER_TARGET_TOKEN must be set in the environment")
		return 2
	}

	bundlePath := *bundleDir
	if strings.TrimSpace(bundlePath) == "" {
		bundlePath = bundle.DirFor(*dir)
	}
	b, err := bundle.Load(bundlePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: publish: load bundle: %v\n", err)
		return 1
	}

	job := events.NewJob()
	err = runJob(job, func() error {
		_, runErr := publish.Run(context.Background(), job, publish.Options{
			Dir:           *dir,
			Manifest:      &b.Manifest,
			Address:       *address,
			TargetBaseURL: *targetURL,
			TargetToken:   keystore.NewSecret(token),
			Owner:         *owner,
			Name:          *name,
			Private:       *private,
			RemoteName:    *remote,
			PublicKeyPath: *sshKey,
		})
		return runErr
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "farrier: publish: %v\n", err)
		return 1
	}
	return 0
}
