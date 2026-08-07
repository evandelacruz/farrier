// Package forge renders Forgejo's own configuration from a Farrier bundle:
// app.ini (FORGE-001), admin bootstrap, fork-PR policy, and CI reconciliation
// land here as their requirement IDs are implemented.
package forge

import (
	"fmt"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/bundle"
)

// Container-side layout of the official Forgejo Docker image. These are
// fixed, not manifest-derived: the Compose definition that ships this
// app.ini (ORCH-002) must mount volumes and run the container consistent
// with these paths.
const (
	dataPath = "/data/gitea"
	dbPath   = dataPath + "/gitea.db"
	lfsPath  = dataPath + "/lfs"
	repoRoot = "/data/git/repositories"
	httpPort = 3000
	sshPort  = 22
	runUser  = "git"
)

// Secrets are the pieces of Forgejo's identity that let app.ini answer every
// question the install wizard would otherwise ask. They are bundle key
// material (spec.md "Identity" > "Key material"): generated once at init and
// carried through every backup and restore. This package only renders them
// into config — it never generates or persists them.
type Secrets struct {
	// SecretKey encrypts sessions and CSRF tokens ([security] SECRET_KEY).
	SecretKey string
	// InternalToken authenticates Forgejo's web process to its internal
	// SSH/API server ([security] INTERNAL_TOKEN).
	InternalToken string
	// LFSJWTSecret signs Git LFS access tokens ([lfs] JWT_SECRET).
	LFSJWTSecret string
}

func (s Secrets) validate() error {
	fields := []struct {
		name  string
		value string
	}{
		{"SecretKey", s.SecretKey},
		{"InternalToken", s.InternalToken},
		{"LFSJWTSecret", s.LFSJWTSecret},
	}
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("forge: %s is required", f.name)
		}
		if strings.ContainsAny(f.value, "\r\n") {
			return fmt.Errorf("forge: %s must not contain newlines", f.name)
		}
	}
	return nil
}

// RenderAppINI renders a complete Forgejo app.ini from a bundle manifest and
// its resolved key material. The rendered file sets INSTALL_LOCK, so
// Forgejo's install wizard is never presented (FORGE-001): every field the
// wizard would ask for — domain, database, repository root, LFS, and the
// identity secrets — is already answered.
//
// The result is deploy-time configuration for the forge host, not bundle
// content: callers must ship it to the host directly and never write it into
// the bundle directory (KEY-003).
func RenderAppINI(m *bundle.Manifest, secrets Secrets) ([]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("forge: manifest is required")
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if err := secrets.validate(); err != nil {
		return nil, err
	}

	domain := strings.TrimSpace(m.Domain)
	rootURL := fmt.Sprintf("https://%s/", domain)

	var b strings.Builder
	fmt.Fprintf(&b, "APP_NAME = Farrier\n")
	fmt.Fprintf(&b, "RUN_MODE = prod\n")
	fmt.Fprintf(&b, "RUN_USER = %s\n\n", runUser)

	fmt.Fprintf(&b, "[server]\n")
	fmt.Fprintf(&b, "PROTOCOL = https\n")
	fmt.Fprintf(&b, "DOMAIN = %s\n", domain)
	fmt.Fprintf(&b, "ROOT_URL = %s\n", rootURL)
	fmt.Fprintf(&b, "HTTP_PORT = %d\n", httpPort)
	fmt.Fprintf(&b, "SSH_DOMAIN = %s\n", domain)
	fmt.Fprintf(&b, "SSH_PORT = %d\n", sshPort)
	fmt.Fprintf(&b, "START_SSH_SERVER = true\n")
	fmt.Fprintf(&b, "LFS_START_SERVER = true\n\n")

	fmt.Fprintf(&b, "[database]\n")
	fmt.Fprintf(&b, "DB_TYPE = sqlite3\n")
	fmt.Fprintf(&b, "PATH = %s\n\n", dbPath)

	fmt.Fprintf(&b, "[repository]\n")
	fmt.Fprintf(&b, "ROOT = %s\n\n", repoRoot)

	fmt.Fprintf(&b, "[security]\n")
	fmt.Fprintf(&b, "INSTALL_LOCK = true\n")
	fmt.Fprintf(&b, "SECRET_KEY = %s\n", secrets.SecretKey)
	fmt.Fprintf(&b, "INTERNAL_TOKEN = %s\n\n", secrets.InternalToken)

	fmt.Fprintf(&b, "[lfs]\n")
	fmt.Fprintf(&b, "PATH = %s\n", lfsPath)
	fmt.Fprintf(&b, "JWT_SECRET = %s\n\n", secrets.LFSJWTSecret)

	fmt.Fprintf(&b, "[actions]\n")
	fmt.Fprintf(&b, "ENABLED = true\n\n")

	fmt.Fprintf(&b, "[log]\n")
	fmt.Fprintf(&b, "MODE = console\n")
	fmt.Fprintf(&b, "LEVEL = info\n")

	return []byte(b.String()), nil
}
