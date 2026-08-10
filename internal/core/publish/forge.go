package publish

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// client is the instance's Forgejo API, reduced to the five calls publish
// makes: who the token belongs to, whether a repository exists, create
// one, delete one, and the account's SSH keys. Nothing here is a general
// Forgejo client — the migration path has its own calls in
// internal/core/importer, and the two deliberately do not share a client
// they would both have to grow.
type client struct {
	baseURL string
	token   keystore.Secret
	http    *http.Client
}

// cleanupTimeout bounds the delete a failed publish issues on its own
// detached context.
const cleanupTimeout = 30 * time.Second

func (c *client) httpClient() *http.Client {
	if c.http == nil {
		return http.DefaultClient
	}
	return c.http
}

// do issues one API call and decodes a 2xx body into out (nil discards
// it). It returns the status code alongside any error so callers can tell
// "absent" from "broken" — a 404 from repoExists is an answer, not a
// failure.
func (c *client) do(ctx context.Context, method, path string, body, out any) (int, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, fmt.Errorf("encode request: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "token "+c.token.Reveal())

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, fmt.Errorf("call the instance: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("the instance returned %d: %s", resp.StatusCode, apiMessage(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, fmt.Errorf("decode response: %w", err)
		}
	}
	return resp.StatusCode, nil
}

// apiMessage prefers Forgejo's own error envelope over the raw body, and
// truncates either way so a stray HTML error page cannot flood an event.
func apiMessage(raw []byte) string {
	var envelope struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &envelope); err == nil && envelope.Message != "" {
		return truncate(envelope.Message)
	}
	return truncate(strings.TrimSpace(string(raw)))
}

func truncate(s string) string {
	const max = 300
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}

// whoami reports the login the target token belongs to. It is what makes
// -owner optional, and it is what tells createRepo whether the owner is
// the token's own account or an organization.
func (c *client) whoami(ctx context.Context) (string, error) {
	var user struct {
		Login string `json:"login"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/user", nil, &user); err != nil {
		return "", fmt.Errorf("identify the token's account: %w", err)
	}
	if user.Login == "" {
		return "", fmt.Errorf("identify the token's account: the instance returned no login")
	}
	return user.Login, nil
}

// repoExists reports whether owner/name is already on the instance. A 404
// is the answer "no", not an error: publish asks before it creates so an
// existing repository fails the run rather than being pushed into.
func (c *client) repoExists(ctx context.Context, owner, name string) (bool, error) {
	status, err := c.do(ctx, http.MethodGet, repoPath(owner, name), nil, nil)
	switch {
	case status == http.StatusNotFound:
		return false, nil
	case err != nil:
		return false, fmt.Errorf("check whether %s/%s exists: %w", owner, name, err)
	default:
		return true, nil
	}
}

type createRepoRequest struct {
	owner         string
	ownerIsSelf   bool
	name          string
	private       bool
	defaultBranch string
}

// createRepoBody is the subset of Forgejo's CreateRepoOption publish sets.
// AutoInit is false and stated rather than omitted: an auto-initialized
// repository carries a commit of its own, and pushing the folder's history
// on top of it would be a rejected non-fast-forward.
type createRepoBody struct {
	Name          string `json:"name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	AutoInit      bool   `json:"auto_init"`
}

// createRepo creates an empty repository for the project, under the
// token's own account or under an organization depending on who the owner
// is, and returns its full name. DefaultBranch is the branch HEAD is on
// locally, so the instance's idea of the project's default branch matches
// the folder's from the first push rather than after a manual correction.
func (c *client) createRepo(ctx context.Context, req createRepoRequest) (string, error) {
	path := "/api/v1/user/repos"
	if !req.ownerIsSelf {
		path = "/api/v1/orgs/" + url.PathEscape(req.owner) + "/repos"
	}

	var created struct {
		FullName string `json:"full_name"`
	}
	if _, err := c.do(ctx, http.MethodPost, path, createRepoBody{
		Name:          req.name,
		Private:       req.private,
		DefaultBranch: req.defaultBranch,
		AutoInit:      false,
	}, &created); err != nil {
		return "", fmt.Errorf("create %s/%s: %w", req.owner, req.name, err)
	}
	if created.FullName == "" {
		return req.owner + "/" + req.name, nil
	}
	return created.FullName, nil
}

// deleteRepo removes owner/name from the instance. It is only ever called
// on a repository this run created, and it runs on its own detached,
// timed-out context rather than the caller's: the failure that triggered
// cleanup may itself be that context expiring, and cleanup still has to
// get its chance. A 404 means there is nothing left to remove.
func (c *client) deleteRepo(owner, name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()

	status, err := c.do(ctx, http.MethodDelete, repoPath(owner, name), nil, nil)
	if status == http.StatusNotFound {
		return nil
	}
	return err
}

func repoPath(owner, name string) string {
	return "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

// sshKey is one public key registered with the instance account.
type sshKey struct {
	Key string `json:"key"`
}

// ensurePushKey makes the authorize step's judgement about whether a push
// from this operator can succeed at all, and reports what it found for the
// step's event.
//
// With a key path, it registers that key unless the account already has
// it — re-running publish for a second project does not accumulate
// duplicate keys. Without one, it only checks: an account with at least
// one key registered is assumed to be the operator's own working setup and
// is left alone, and an account with none fails here, before anything is
// created, rather than as a permission-denied push two steps later.
func ensurePushKey(ctx context.Context, c *client, user, publicKeyPath string) (string, error) {
	var keys []sshKey
	if _, err := c.do(ctx, http.MethodGet, "/api/v1/user/keys", nil, &keys); err != nil {
		return "", fmt.Errorf("list the account's ssh keys: %w", err)
	}

	if publicKeyPath == "" {
		if len(keys) == 0 {
			return "", fmt.Errorf("the account %s has no ssh public key registered, so a push would be rejected: re-run with the path to your public key, or add it to the account in the forge's web UI", user)
		}
		return fmt.Sprintf("%d ssh key(s) already registered", len(keys)), nil
	}

	raw, err := os.ReadFile(publicKeyPath)
	if err != nil {
		return "", fmt.Errorf("read ssh public key: %w", err)
	}
	keyType, blob, err := bundle.SplitSSHPublicKey(string(raw))
	if err != nil {
		return "", fmt.Errorf("%s %w", publicKeyPath, err)
	}

	for _, existing := range keys {
		existingType, existingBlob, err := bundle.SplitSSHPublicKey(existing.Key)
		if err != nil {
			continue
		}
		if existingType == keyType && existingBlob == blob {
			return fmt.Sprintf("%s is already registered", publicKeyPath), nil
		}
	}

	body := struct {
		Title string `json:"title"`
		Key   string `json:"key"`
	}{Title: keyTitle(string(raw), blob), Key: keyType + " " + blob}
	if _, err := c.do(ctx, http.MethodPost, "/api/v1/user/keys", body, nil); err != nil {
		return "", fmt.Errorf("register ssh public key with %s: %w", user, err)
	}
	return fmt.Sprintf("registered %s", publicKeyPath), nil
}

// keyTitle names a registered key in the forge's UI: the key's own comment
// when it has one, since that is what the operator recognises, and a short
// digest of the key otherwise so two different keys never collide on one
// title.
func keyTitle(line, blob string) string {
	if fields := strings.Fields(strings.TrimSpace(line)); len(fields) > 2 {
		return "farrier: " + strings.Join(fields[2:], " ")
	}
	sum := sha256.Sum256([]byte(blob))
	return "farrier: " + hex.EncodeToString(sum[:])[:12]
}
