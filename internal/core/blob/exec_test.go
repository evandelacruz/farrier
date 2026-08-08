package blob

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeInvoker plays the part of a driver executable in-process: it
// implements driver.Invoker directly against an in-memory store, so these
// tests exercise ExecAdapter's wiring (params encoding, temp-file
// staging, result decoding) without spawning a real subprocess — that's
// driver.Exec's own test, in internal/core/driver.
type fakeInvoker struct {
	store map[string][]byte
}

func newFakeInvoker() *fakeInvoker {
	return &fakeInvoker{store: map[string][]byte{}}
}

func (f *fakeInvoker) Invoke(ctx context.Context, method string, params, result any) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}

	var out any
	switch method {
	case "list":
		var p execListParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		var objs []Object
		for k, v := range f.store {
			if strings.HasPrefix(k, p.Prefix) {
				objs = append(objs, Object{Key: k, Size: int64(len(v))})
			}
		}
		out = execListResult{Objects: objs}
	case "get":
		var p execGetParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		content, ok := f.store[p.Key]
		if !ok {
			out = execGetResult{NotFound: true}
			break
		}
		if err := os.WriteFile(p.Path, content, 0o600); err != nil {
			return err
		}
		out = execGetResult{}
	case "put":
		var p execPutParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		content, err := os.ReadFile(p.Path)
		if err != nil {
			return err
		}
		f.store[p.Key] = content
		return nil
	default:
		return errors.New("fakeInvoker: unknown method " + method)
	}

	if result == nil {
		return nil
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, result)
}

func TestExecPutGetRoundTrip(t *testing.T) {
	a := NewExec(newFakeInvoker())
	ctx := context.Background()

	content := []byte("hello, exec blob")
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

func TestExecGetMissingKeyReturnsErrNotFound(t *testing.T) {
	a := NewExec(newFakeInvoker())

	_, err := a.Get(context.Background(), "does/not/exist")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
}

func TestExecList(t *testing.T) {
	a := NewExec(newFakeInvoker())
	ctx := context.Background()

	for _, k := range []string{"lfs/a", "lfs/b", "artifacts/c"} {
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

// rawInvoker returns a fixed, hand-written JSON response for every call —
// a stand-in for a third-party driver executable's literal stdout, to
// exercise how ExecAdapter.List decodes a "modified" field a real
// (non-Go) driver would produce.
type rawInvoker struct {
	response string
}

func (r rawInvoker) Invoke(ctx context.Context, method string, params, result any) error {
	return json.Unmarshal([]byte(r.response), result)
}

func TestExecListDecodesLowercaseModifiedField(t *testing.T) {
	a := NewExec(rawInvoker{response: `{"objects":[{"key":"a","size":3,"modified":"2024-01-02T03:04:05Z"}]}`})

	objects, err := a.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("List returned %d objects, want 1", len(objects))
	}
	want, _ := time.Parse(time.RFC3339, "2024-01-02T03:04:05Z")
	if !objects[0].Modified.Equal(want) {
		t.Fatalf("Modified = %v, want %v", objects[0].Modified, want)
	}
}

func TestExecListOmittedModifiedFieldIsZero(t *testing.T) {
	a := NewExec(rawInvoker{response: `{"objects":[{"key":"a","size":3}]}`})

	objects, err := a.List(context.Background(), "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(objects) != 1 {
		t.Fatalf("List returned %d objects, want 1", len(objects))
	}
	if !objects[0].Modified.IsZero() {
		t.Fatalf("Modified = %v, want zero value (older driver, field omitted)", objects[0].Modified)
	}
}

func TestExecGetDoesNotLeaveTempFileAfterClose(t *testing.T) {
	a := NewExec(newFakeInvoker())
	ctx := context.Background()

	content := []byte("streamed content")
	if err := a.Put(ctx, "k", bytes.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	rc, err := a.Get(ctx, "k")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	dc, ok := rc.(*deleteOnCloseFile)
	if !ok {
		t.Fatalf("Get returned %T, want *deleteOnCloseFile", rc)
	}
	path := dc.path
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temp file %s still exists after Close", path)
	}
}
