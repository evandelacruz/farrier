// Package forge implements FORGE-002: provisioning the forge's first admin
// account during deployment and handing its credentials to the operator
// exactly once, through the CORE-002 job event stream — the only place
// they are ever emitted.
package forge

import (
	"crypto/rand"
	"fmt"
	"strings"

	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// Service is the Compose service name of the forgejo container, matching
// the manifest's "forgejo" image key (CORE-001) — the target of the admin
// bootstrap command.
const Service = "forgejo"

const adminUsername = "admin"
const passwordLength = 24

// passwordCharset is 64 characters — an exact divisor of 256 — so mapping
// a random byte onto it with % introduces no modulo bias. Excludes shell
// metacharacters and quote characters so the generated password never
// needs escaping.
const passwordCharset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"

// AdminAccount is the forge's first admin account: a fixed username, an
// email derived from the bundle domain, and a password Farrier generates
// fresh for every deployment. Password is a keystore.Secret (KEY-003) so
// the account never prints its password via an accidental %v, log line, or
// JSON/YAML marshal — defense in depth even though the password isn't key
// material by spec.
type AdminAccount struct {
	Username string
	Email    string
	Password keystore.Secret
}

// NewAdminAccount generates the first admin account for a forge at domain:
// username "admin", email admin@domain, and a fresh random password. Every
// call returns its own password — none is ever reused or derived from
// anything else.
func NewAdminAccount(domain string) (AdminAccount, error) {
	if strings.TrimSpace(domain) == "" {
		return AdminAccount{}, fmt.Errorf("forge: domain is required")
	}
	password, err := randomPassword(passwordLength)
	if err != nil {
		return AdminAccount{}, fmt.Errorf("forge: generate admin password: %w", err)
	}
	return AdminAccount{
		Username: adminUsername,
		Email:    "admin@" + domain,
		Password: keystore.NewSecret(password),
	}, nil
}

func randomPassword(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, n)
	for i, b := range buf {
		out[i] = passwordCharset[int(b)%len(passwordCharset)]
	}
	return string(out), nil
}
