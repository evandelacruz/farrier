package keystore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/driver"
)

// fakeInvoker is a driver.Invoker test double that records the call it
// received and serves a canned result or error, without spawning a
// process.
type fakeInvoker struct {
	gotMethod string
	gotParams any
	result    any
	err       error
}

func (f *fakeInvoker) Invoke(ctx context.Context, method string, params, result any) error {
	f.gotMethod = method
	f.gotParams = params
	if f.err != nil {
		return f.err
	}
	data, err := json.Marshal(f.result)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, result)
}

func TestExecDriverResolveDecodesBase64Secret(t *testing.T) {
	secret := []byte("hunter2")
	fake := &fakeInvoker{result: execResolveResult{Secret: base64.StdEncoding.EncodeToString(secret)}}
	d := execDriver{invoker: fake}

	got, err := d.Resolve(context.Background(), "forgejo_secret_key")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if string(got) != "hunter2" {
		t.Fatalf("Resolve = %q, want %q", got, "hunter2")
	}
	if fake.gotMethod != "resolve" {
		t.Fatalf("method = %q, want %q", fake.gotMethod, "resolve")
	}
	params, ok := fake.gotParams.(execResolveParams)
	if !ok || params.Key != "forgejo_secret_key" {
		t.Fatalf("params = %+v, want Key=forgejo_secret_key", fake.gotParams)
	}
}

func TestExecDriverResolveInvokerErrorPropagates(t *testing.T) {
	fake := &fakeInvoker{err: errors.New("boom")}
	d := execDriver{invoker: fake}

	_, err := d.Resolve(context.Background(), "k")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Resolve error = %v, want it to contain %q", err, "boom")
	}
}

func TestExecDriverResolveInvalidBase64Errors(t *testing.T) {
	fake := &fakeInvoker{result: execResolveResult{Secret: "not-valid-base64!!"}}
	d := execDriver{invoker: fake}

	if _, err := d.Resolve(context.Background(), "k"); err == nil {
		t.Fatal("Resolve: want error for invalid base64, got nil")
	}
}

func TestExecDriverResolveEmptySecretErrors(t *testing.T) {
	fake := &fakeInvoker{result: execResolveResult{Secret: ""}}
	d := execDriver{invoker: fake}

	if _, err := d.Resolve(context.Background(), "k"); err == nil {
		t.Fatal("Resolve: want error for empty secret, got nil")
	}
}

func TestExecDriverResolveEmptyKeyNameErrors(t *testing.T) {
	fake := &fakeInvoker{}
	d := execDriver{invoker: fake}

	if _, err := d.Resolve(context.Background(), ""); err == nil {
		t.Fatal("Resolve: want error for empty key name, got nil")
	}
	if fake.gotMethod != "" {
		t.Fatal("Resolve: invoker should not be called for an empty key name")
	}
}

func TestNewExecDriverRequiresPath(t *testing.T) {
	if _, err := New("onepassword", map[string]any{}); err == nil {
		t.Fatal("New: want error for missing config.path, got nil")
	}
}

func TestNewExecDriverParsesPathAndArgs(t *testing.T) {
	got, err := New("onepassword", map[string]any{
		"path": "/usr/local/bin/farrier-keystore-onepassword",
		"args": []any{"--vault", "prod"},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ed, ok := got.(execDriver)
	if !ok {
		t.Fatalf("New = %T, want execDriver", got)
	}
	exe, ok := ed.invoker.(driver.Exec)
	if !ok {
		t.Fatalf("execDriver.invoker = %T, want driver.Exec", ed.invoker)
	}
	if exe.Path != "/usr/local/bin/farrier-keystore-onepassword" {
		t.Fatalf("Path = %q, want %q", exe.Path, "/usr/local/bin/farrier-keystore-onepassword")
	}
	if len(exe.Args) != 2 || exe.Args[0] != "--vault" || exe.Args[1] != "prod" {
		t.Fatalf("Args = %v, want [--vault prod]", exe.Args)
	}
}

func TestNewExecDriverRejectsNonStringArgs(t *testing.T) {
	_, err := New("onepassword", map[string]any{
		"path": "/usr/local/bin/farrier-keystore-onepassword",
		"args": []any{1, 2},
	})
	if err == nil {
		t.Fatal("New: want error for non-string args, got nil")
	}
}
