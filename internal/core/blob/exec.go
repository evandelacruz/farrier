package blob

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/evandelacruz/farrier/internal/core/driver"
)

// ExecAdapter is the out-of-tree blob adapter (CORE-003): it satisfies
// Adapter by invoking a driver executable through the exec protocol
// instead of talking to a backing store directly, so a third party can
// ship a blob adapter as a standalone executable without linking Go —
// the same posture used for dns and keystore drivers (tech-spec.md
// "Driver interfaces").
//
// List's result is small (object metadata) and travels in the exec
// protocol's JSON envelope directly. Get and Put don't: LFS objects and CI
// artifacts can run to multiple gigabytes, and base64-encoding that through
// a JSON Request/Response would be impractical. Instead, ExecAdapter
// stages content through a local temp file and passes its path in Params;
// the driver executable reads or writes that path directly rather than
// exchanging bytes over stdin/stdout. This relies on driver.Exec always
// running the driver executable as a local child process, so it shares a
// filesystem with ExecAdapter.
type ExecAdapter struct {
	Invoker driver.Invoker
}

// NewExec returns an ExecAdapter that calls through invoker.
func NewExec(invoker driver.Invoker) *ExecAdapter {
	return &ExecAdapter{Invoker: invoker}
}

type execListParams struct {
	Prefix string `json:"prefix"`
}

type execListResult struct {
	Objects []Object `json:"objects"`
}

// List invokes the "list" method and decodes its result directly, since
// object metadata is small enough for the JSON envelope. Each returned
// object carries a "modified" field (Object.Modified) alongside "key" and
// "size" — a driver executable that predates the field simply omits it,
// which decodes as the zero time, meaning unknown rather than very old.
func (a *ExecAdapter) List(ctx context.Context, prefix string) ([]Object, error) {
	var res execListResult
	if err := a.Invoker.Invoke(ctx, "list", execListParams{Prefix: prefix}, &res); err != nil {
		return nil, fmt.Errorf("blob: exec: list: %w", err)
	}
	return res.Objects, nil
}

type execGetParams struct {
	Key  string `json:"key"`
	Path string `json:"path"`
}

type execGetResult struct {
	NotFound bool `json:"notFound"`
}

// Get invokes the "get" method, directing the driver executable to write
// the object's content to a local temp file instead of returning it
// inline. The returned reader owns that temp file and removes it on
// Close.
func (a *ExecAdapter) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	tmp, err := os.CreateTemp("", "blob-exec-get-*")
	if err != nil {
		return nil, fmt.Errorf("blob: exec: get %s: %w", key, err)
	}
	tmpPath := tmp.Name()
	tmp.Close()

	var res execGetResult
	if err := a.Invoker.Invoke(ctx, "get", execGetParams{Key: key, Path: tmpPath}, &res); err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("blob: exec: get %s: %w", key, err)
	}
	if res.NotFound {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("blob: exec: get %s: %w", key, ErrNotFound)
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return nil, fmt.Errorf("blob: exec: get %s: %w", key, err)
	}
	return &deleteOnCloseFile{File: f, path: tmpPath}, nil
}

type execPutParams struct {
	Key  string `json:"key"`
	Path string `json:"path"`
	Size int64  `json:"size"`
}

// Put stages r in a local temp file, then invokes the "put" method with
// that file's path so the driver executable reads the content directly
// rather than through the JSON envelope.
func (a *ExecAdapter) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	tmp, err := os.CreateTemp("", "blob-exec-put-*")
	if err != nil {
		return fmt.Errorf("blob: exec: put %s: %w", key, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	if _, err := io.Copy(tmp, r); err != nil {
		tmp.Close()
		return fmt.Errorf("blob: exec: put %s: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("blob: exec: put %s: %w", key, err)
	}

	if err := a.Invoker.Invoke(ctx, "put", execPutParams{Key: key, Path: tmpPath, Size: size}, nil); err != nil {
		return fmt.Errorf("blob: exec: put %s: %w", key, err)
	}
	return nil
}

// deleteOnCloseFile removes its backing file from disk when Close is
// called, so a Get caller's temp file never outlives the returned reader.
type deleteOnCloseFile struct {
	*os.File
	path string
}

func (f *deleteOnCloseFile) Close() error {
	closeErr := f.File.Close()
	if err := os.Remove(f.path); err != nil && closeErr == nil {
		return err
	}
	return closeErr
}
