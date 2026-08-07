package registry

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSplitRef(t *testing.T) {
	cases := []struct {
		ref            string
		name, tag, dig string
	}{
		{"caddy", "caddy", "latest", ""},
		{"caddy:2", "caddy", "2", ""},
		{"docker.io/library/caddy:2", "docker.io/library/caddy", "2", ""},
		{"localhost:5000/caddy:2", "localhost:5000/caddy", "2", ""},
		{"localhost:5000/caddy", "localhost:5000/caddy", "latest", ""},
		{"caddy@sha256:" + strings.Repeat("a", 64), "caddy", "", "sha256:" + strings.Repeat("a", 64)},
		{"caddy:2@sha256:" + strings.Repeat("a", 64), "caddy:2", "", "sha256:" + strings.Repeat("a", 64)},
	}
	for _, c := range cases {
		name, tag, dig := splitRef(c.ref)
		if name != c.name || tag != c.tag || dig != c.dig {
			t.Errorf("splitRef(%q) = (%q, %q, %q), want (%q, %q, %q)", c.ref, name, tag, dig, c.name, c.tag, c.dig)
		}
	}
}

func TestSplitName(t *testing.T) {
	cases := []struct {
		name       string
		host, repo string
	}{
		{"caddy", "docker.io", "library/caddy"},
		{"forgejo/forgejo", "docker.io", "forgejo/forgejo"},
		{"codeberg.org/forgejo/forgejo", "codeberg.org", "forgejo/forgejo"},
		{"localhost:5000/caddy", "localhost:5000", "caddy"},
		{"localhost/caddy", "localhost", "caddy"},
	}
	for _, c := range cases {
		host, repo := splitName(c.name)
		if host != c.host || repo != c.repo {
			t.Errorf("splitName(%q) = (%q, %q), want (%q, %q)", c.name, host, repo, c.host, c.repo)
		}
	}
}

func TestResolveAlreadyPinnedIsUnchanged(t *testing.T) {
	ref := "codeberg.org/forgejo/forgejo@sha256:" + strings.Repeat("a", 64)
	got, err := Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != ref {
		t.Errorf("Resolve(%q) = %q, want unchanged", ref, got)
	}
}

func TestResolveRejectsEmptyRef(t *testing.T) {
	if _, err := Resolve(context.Background(), "  "); err == nil {
		t.Fatal("Resolve: want error for empty ref, got nil")
	}
}

// registryServer builds a minimal OCI-distribution-compatible server: the
// first anonymous manifest request gets a Bearer challenge, the token
// endpoint hands back a fixed token, and only a request carrying that token
// gets the manifest with its digest header set.
func registryServer(t *testing.T, digest string) *httptest.Server {
	t.Helper()
	const wantToken = "test-token"

	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"token":%q}`, wantToken)
	})
	mux.HandleFunc("/v2/myorg/myimage/manifests/", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer "+wantToken {
			w.Header().Set("WWW-Authenticate", fmt.Sprintf(
				`Bearer realm="%s/token",service="test-registry",scope="repository:myorg/myimage:pull"`,
				serverBaseURL(r)))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Docker-Content-Digest", digest)
		w.Header().Set("Content-Type", "application/vnd.oci.image.index.v1+json")
		w.Write([]byte(`{"schemaVersion":2}`))
	})

	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// serverBaseURL is filled in after the server starts (its URL isn't known
// until then), so the realm in the 401 challenge points back at this same
// test server rather than a real one.
var testServerURL string

func serverBaseURL(r *http.Request) string {
	return testServerURL
}

func TestResolveFetchesAndPinsDigest(t *testing.T) {
	digest := "sha256:" + strings.Repeat("b", 64)
	srv := registryServer(t, digest)
	testServerURL = srv.URL

	host := strings.TrimPrefix(srv.URL, "https://")
	ref := host + "/myorg/myimage:v1"

	r := Resolver{Client: srv.Client()}
	got, err := r.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := host + "/myorg/myimage@" + digest
	if got != want {
		t.Errorf("Resolve(%q) = %q, want %q", ref, got, want)
	}
}

func TestResolveDefaultsToLatestTag(t *testing.T) {
	digest := "sha256:" + strings.Repeat("c", 64)
	srv := registryServer(t, digest)
	testServerURL = srv.URL

	host := strings.TrimPrefix(srv.URL, "https://")
	ref := host + "/myorg/myimage"

	r := Resolver{Client: srv.Client()}
	got, err := r.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := host + "/myorg/myimage@" + digest
	if got != want {
		t.Errorf("Resolve(%q) = %q, want %q", ref, got, want)
	}
}

func TestResolveFailsOnRegistryError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/myorg/myimage/manifests/latest", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)

	host := strings.TrimPrefix(srv.URL, "https://")
	r := Resolver{Client: srv.Client()}
	if _, err := r.Resolve(context.Background(), host+"/myorg/myimage"); err == nil {
		t.Fatal("Resolve: want error for 404, got nil")
	}
}
