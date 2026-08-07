// Package registry resolves a container image reference to a digest-pinned
// one, against any registry implementing the standard OCI/Docker
// distribution HTTP API (GET /v2/<repo>/manifests/<tag>, an optional Bearer
// challenge via WWW-Authenticate).
//
// A bundle manifest's images must be pinned by digest, never a tag
// (tech-spec.md "Bundle directory"): tags float, so a backup or restore
// running months later would silently run different bytes than the version
// that was actually verified. Resolve is what lets an operator hand init a
// human-friendly reference — a bare image name, or name:tag — and get back
// the pinned form the manifest actually stores, without looking up a SHA-256
// digest by hand.
package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// acceptManifestTypes lists every manifest media type Resolve is willing to
// pin: multi-arch indexes (OCI and Docker's own manifest-list predecessor)
// first, since most published images are multi-arch, then single-platform
// manifests as a fallback for registries that publish only those.
const acceptManifestTypes = "application/vnd.oci.image.index.v1+json, " +
	"application/vnd.docker.distribution.manifest.list.v2+json, " +
	"application/vnd.oci.image.manifest.v1+json, " +
	"application/vnd.docker.distribution.manifest.v2+json"

// Resolver resolves image references against real registries over HTTPS.
// The zero value is ready to use.
type Resolver struct {
	// Client makes every HTTP request; nil uses http.DefaultClient. Tests
	// point this at an httptest.Server's client to avoid real network
	// calls.
	Client *http.Client
}

// Resolve pins ref to a specific digest: "name@sha256:<hex>". If ref already
// carries a digest, it is returned unchanged — Resolve is then just a
// no-op validation that ref parses. Otherwise ref's tag (or "latest", if
// none is given) is looked up against the image's registry and replaced
// with the digest the registry currently serves for it.
func Resolve(ctx context.Context, ref string) (string, error) {
	return Resolver{}.Resolve(ctx, ref)
}

// Resolve is the Resolver method Resolve(ctx, ref) delegates to.
func (r Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	if strings.TrimSpace(ref) == "" {
		return "", fmt.Errorf("registry: image reference is required")
	}

	name, tag, digest := splitRef(ref)
	if digest != "" {
		return name + "@" + digest, nil
	}

	host, repo := splitName(name)
	apiHost := host
	if host == "docker.io" {
		apiHost = "registry-1.docker.io"
	}

	resolved, err := r.fetchDigest(ctx, apiHost, repo, tag)
	if err != nil {
		return "", fmt.Errorf("registry: resolve %s: %w", ref, err)
	}
	return fmt.Sprintf("%s/%s@%s", host, repo, resolved), nil
}

// splitRef splits ref into its name and, exclusively, either a tag or a
// digest. A digest (anything after "@") takes precedence over any tag, since
// "name:tag@sha256:..." is valid and the digest is authoritative. Absent
// both, tag defaults to "latest" — the same default `docker pull` applies.
func splitRef(ref string) (name, tag, digest string) {
	if i := strings.Index(ref, "@"); i != -1 {
		return ref[:i], "", ref[i+1:]
	}
	lastSlash := strings.LastIndex(ref, "/")
	rest := ref[lastSlash+1:]
	if ci := strings.LastIndex(rest, ":"); ci != -1 {
		return ref[:lastSlash+1] + rest[:ci], rest[ci+1:], ""
	}
	return ref, "latest", ""
}

// splitName splits an image name into its registry host and repository
// path, applying Docker Hub's implicit-registry convention: a name with no
// registry host (its first path segment has no "." or ":", and isn't
// "localhost") is a Docker Hub image, and a name with no "/" at all is one
// of Docker Hub's official images, addressed as "library/<name>".
func splitName(name string) (host, repo string) {
	i := strings.Index(name, "/")
	if i == -1 {
		return "docker.io", "library/" + name
	}
	first := name[:i]
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first, name[i+1:]
	}
	return "docker.io", name
}

// fetchDigest looks up the manifest for repo:tag on apiHost and returns its
// digest, retrying once with a Bearer token if the registry challenges the
// anonymous request (the standard flow: Docker Hub and most registries
// require it even for public images).
func (r Resolver) fetchDigest(ctx context.Context, apiHost, repo, tag string) (string, error) {
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}

	manifestURL := fmt.Sprintf("https://%s/v2/%s/manifests/%s", apiHost, repo, tag)

	resp, err := r.getManifest(ctx, client, manifestURL, "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized {
		token, err := authenticate(ctx, client, resp.Header.Get("WWW-Authenticate"))
		if err != nil {
			return "", fmt.Errorf("authenticate: %w", err)
		}
		resp.Body.Close()
		resp, err = r.getManifest(ctx, client, manifestURL, token)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read manifest: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("registry returned %d for %s/%s:%s: %s", resp.StatusCode, apiHost, repo, tag, trim(body))
	}

	if digest := resp.Header.Get("Docker-Content-Digest"); digest != "" {
		return digest, nil
	}
	// A registry that omits the digest header still lets us compute it
	// ourselves: the digest of an OCI/Docker manifest is defined as the
	// SHA-256 of its exact response body.
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func (r Resolver) getManifest(ctx context.Context, client *http.Client, manifestURL, token string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", acceptManifestTypes)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest: %w", err)
	}
	return resp, nil
}

var authParamPattern = regexp.MustCompile(`(realm|service|scope)="([^"]*)"`)

// authenticate exchanges a WWW-Authenticate Bearer challenge for the token
// it names, following the anonymous-token flow every major registry (Docker
// Hub, GHCR, Codeberg's own) uses for public image pulls: no credentials are
// sent, only the challenge's own realm/service/scope.
func authenticate(ctx context.Context, client *http.Client, challenge string) (string, error) {
	if !strings.HasPrefix(strings.ToLower(challenge), "bearer ") {
		return "", fmt.Errorf("unsupported auth challenge: %q", challenge)
	}

	params := map[string]string{}
	for _, m := range authParamPattern.FindAllStringSubmatch(challenge, -1) {
		params[m[1]] = m[2]
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("auth challenge missing realm: %q", challenge)
	}

	q := url.Values{}
	if service := params["service"]; service != "" {
		q.Set("service", service)
	}
	if scope := params["scope"]; scope != "" {
		q.Set("scope", scope)
	}

	tokenURL := realm
	if encoded := q.Encode(); encoded != "" {
		tokenURL += "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch token: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, trim(body))
	}

	token, err := extractToken(body)
	if err != nil {
		return "", err
	}
	return token, nil
}

// tokenResponse covers both keys registries use: "token" is the spec's own
// name, "access_token" is Docker Hub's (older, still served alongside
// "token" for compatibility). Either means the same thing.
type tokenResponse struct {
	Token       string `json:"token"`
	AccessToken string `json:"access_token"`
}

func extractToken(body []byte) (string, error) {
	var t tokenResponse
	if err := json.Unmarshal(body, &t); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if t.Token != "" {
		return t.Token, nil
	}
	if t.AccessToken != "" {
		return t.AccessToken, nil
	}
	return "", fmt.Errorf("token response carried no token: %s", trim(body))
}

func trim(body []byte) string {
	const max = 200
	s := strings.TrimSpace(string(body))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
