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

// adminUsername is the login name of the forge's first admin account, and
// also the owner of the scratch repository the drill's smoke job creates
// (see smoke.go) — the two are the same account and must stay the same
// string.
//
// It is deliberately not "admin". Forgejo reserves that name, and
// `forgejo admin user create --username admin` fails outright with
// "CreateUser: name is reserved [name: admin]", which left `up` unable to
// provision its first admin on any host (found on a real deployment,
// 2026-08-10). "farrier" is confirmed accepted by Forgejo and says which
// tool created the account. Anything changing it must be checked against a
// running Forgejo: the reserved list is upstream data, not something this
// repository can assert.
const adminUsername = "farrier"
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
// the fixed username above, that same name @domain as the email, and a
// fresh random password. Every call returns its own password — none is ever
// reused or derived from anything else.
//
// The email's local part is the username rather than a second literal, so
// there is one name to change and no way for the two to drift apart. Nothing
// constrains it otherwise: Forgejo reserves usernames, not addresses.
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
		Email:    adminUsername + "@" + domain,
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
