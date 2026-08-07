package blob

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// fakeS3 is a minimal S3-compatible server: enough of the REST API
// (path-style PUT/GET/HEAD on an object, ListObjectsV2 on a bucket) for
// S3Adapter's own requests. It does not verify request signatures — the
// point is to exercise S3Adapter's request shapes and response handling,
// not minio-go's signer.
type fakeS3 struct {
	mu      sync.Mutex
	bucket  string
	objects map[string][]byte
}

func newFakeS3(bucket string) *fakeS3 {
	return &fakeS3{bucket: bucket, objects: make(map[string][]byte)}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucketPrefix := f.bucket + "/"

	if strings.TrimSuffix(path, "/") == f.bucket && r.URL.Query().Get("list-type") == "2" {
		f.list(w, r)
		return
	}
	if !strings.HasPrefix(path, bucketPrefix) {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	key := strings.TrimPrefix(path, bucketPrefix)

	f.mu.Lock()
	defer f.mu.Unlock()

	switch r.Method {
	case http.MethodPut:
		body, err := io.ReadAll(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		f.objects[key] = body
		w.Header().Set("ETag", `"fake"`)
		w.Header().Set("Last-Modified", f.now())
		w.WriteHeader(http.StatusOK)
	case http.MethodHead:
		content, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Header().Set("Last-Modified", f.now())
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		content, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.Header().Set("Last-Modified", f.now())
		w.WriteHeader(http.StatusOK)
		w.Write(content)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// now returns the current time formatted as an HTTP date, the format
// minio-go's Stat parses Last-Modified with.
func (f *fakeS3) now() string {
	return time.Now().UTC().Format(http.TimeFormat)
}

type listContent struct {
	Key  string `xml:"Key"`
	Size int64  `xml:"Size"`
}

type listBucketResult struct {
	XMLName  xml.Name      `xml:"ListBucketResult"`
	Name     string        `xml:"Name"`
	Prefix   string        `xml:"Prefix"`
	KeyCount int           `xml:"KeyCount"`
	MaxKeys  int           `xml:"MaxKeys"`
	Contents []listContent `xml:"Contents"`
}

func (f *fakeS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")

	f.mu.Lock()
	result := listBucketResult{Name: f.bucket, Prefix: prefix, MaxKeys: 1000}
	for key, content := range f.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		result.Contents = append(result.Contents, listContent{Key: key, Size: int64(len(content))})
	}
	f.mu.Unlock()
	result.KeyCount = len(result.Contents)

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	enc := xml.NewEncoder(w)
	enc.Encode(result)
}

func newTestS3Adapter(t *testing.T, bucket string) (*S3Adapter, *fakeS3) {
	t.Helper()
	fake := newFakeS3(bucket)
	server := httptest.NewTLSServer(fake)
	t.Cleanup(server.Close)

	client, err := minio.New(strings.TrimPrefix(server.URL, "https://"), &minio.Options{
		Creds:        credentials.NewStaticV4("test-access-key", "test-secret-key", ""),
		Secure:       true,
		Region:       "us-east-1",
		Transport:    server.Client().Transport,
		BucketLookup: minio.BucketLookupPath,
	})
	if err != nil {
		t.Fatalf("minio.New: %v", err)
	}
	return &S3Adapter{client: client, bucket: bucket}, fake
}

func TestS3PutGetRoundTrip(t *testing.T) {
	a, _ := newTestS3Adapter(t, "test-bucket")
	ctx := context.Background()

	content := []byte("hello, s3")
	if err := a.Put(ctx, "objects/a.bin", bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := a.Get(ctx, "objects/a.bin")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("got %q, want %q", got, content)
	}
}

func TestS3GetMissingKeyReturnsErrNotFound(t *testing.T) {
	a, _ := newTestS3Adapter(t, "test-bucket")

	_, err := a.Get(context.Background(), "does/not/exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestS3List(t *testing.T) {
	a, _ := newTestS3Adapter(t, "test-bucket")
	ctx := context.Background()

	keys := []string{"lfs/a", "lfs/b", "artifacts/c"}
	for _, k := range keys {
		if err := a.Put(ctx, k, bytes.NewReader([]byte(k)), int64(len(k))); err != nil {
			t.Fatalf("Put %s: %v", k, err)
		}
	}

	objects, err := a.List(ctx, "lfs/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := make([]string, len(objects))
	for i, o := range objects {
		got[i] = o.Key
	}
	sort.Strings(got)
	want := []string{"lfs/a", "lfs/b"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("List(%q) = %v, want %v", "lfs/", got, want)
	}
}

func TestNewS3RequiresEndpointAndBucket(t *testing.T) {
	if _, err := NewS3(S3Config{Bucket: "b"}); err == nil {
		t.Fatal("NewS3 with no endpoint: want error, got nil")
	}
	if _, err := NewS3(S3Config{Endpoint: "example.com"}); err == nil {
		t.Fatal("NewS3 with no bucket: want error, got nil")
	}
}
