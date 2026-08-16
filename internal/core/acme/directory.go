package acme

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/go-acme/lego/v4/lego"
)

// ProductionDirectoryURL and StagingDirectoryURL are Let's Encrypt's two
// ACME directory endpoints. Production is what an unset choice resolves to
// — it is the CA that issues certificates browsers trust, and the one every
// real deployment wants.
//
// Staging speaks the identical protocol and issues from an untrusted root,
// which is exactly why it is worth reaching: its rate limits are far higher
// than production's, so the first runs of a named deployment — the runs
// most likely to hit a defect and be repeated — can be rehearsed without
// spending a production quota that resets weekly.
const (
	ProductionDirectoryURL = lego.LEDirectoryProduction
	StagingDirectoryURL    = lego.LEDirectoryStaging
)

// StagingShorthand is the value an operator may give instead of pasting
// StagingDirectoryURL. Rehearsing against Let's Encrypt staging is the case
// this setting exists for, and a URL nobody can recall from memory is a
// poor way to spell the common answer.
const StagingShorthand = "staging"

// ResolveDirectoryURL turns an operator's ACME server choice into the
// directory URL to issue against: empty gives Let's Encrypt production,
// StagingShorthand gives Let's Encrypt staging, and anything else must be
// an absolute http or https URL, which is returned unchanged. A URL rather
// than a staging/production switch, because the same field then reaches an
// internal ACME CA — step-ca and its kind — which is a real deployment for
// a private team, and one setting cannot disagree with itself the way two
// would.
//
// Callers resolve once, at the point the operator's input arrives, and
// record the result: what a certificate was issued by decides what it must
// be renewed by, and a shorthand stored verbatim is a value whose meaning
// depends on the code that reads it. Resolving here is also what makes a
// malformed URL a refusal on the operator's machine rather than a failed
// ACME exchange on a host.
func ResolveDirectoryURL(value string) (string, error) {
	v := strings.TrimSpace(value)
	switch v {
	case "":
		return ProductionDirectoryURL, nil
	case StagingShorthand:
		return StagingDirectoryURL, nil
	}

	parsed, err := url.Parse(v)
	if err != nil {
		return "", fmt.Errorf("acme: directory %q is not a URL: %w", value, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("acme: directory %q must be an http or https URL, or %q for Let's Encrypt's staging environment", value, StagingShorthand)
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("acme: directory %q names no host", value)
	}
	return v, nil
}
