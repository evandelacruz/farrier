package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/blob"
	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/keystore"
	"github.com/evandelacruz/farrier/internal/core/state"
)

type fakeGitExporter struct {
	remotes []state.Remote
	err     error
}

func (f *fakeGitExporter) Remotes(ctx context.Context) ([]state.Remote, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.remotes, nil
}

type fakeGitCapturer struct {
	content map[string][]byte
	err     error
	errFor  string
	calls   []string

	refsContent map[string][]byte
	refsErr     error
	refsErrFor  string
	refsCalls   []string

	// order, when set, also gets an "archive:<name>"/"refs:<name>" entry
	// appended for every call, interleaved with a fakePushHold sharing the
	// same slice so a test can assert relative ordering across both.
	order *[]string
}

func (f *fakeGitCapturer) Archive(ctx context.Context, remote state.Remote) (io.ReadCloser, error) {
	f.calls = append(f.calls, remote.Name)
	if f.order != nil {
		*f.order = append(*f.order, "archive:"+remote.Name)
	}
	if f.err != nil && (f.errFor == "" || f.errFor == remote.Name) {
		return nil, f.err
	}
	data := f.content[remote.Name]
	if data == nil {
		data = []byte("tar-bytes-" + remote.Name)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (f *fakeGitCapturer) Refs(ctx context.Context, remote state.Remote) (io.ReadCloser, error) {
	f.refsCalls = append(f.refsCalls, remote.Name)
	if f.order != nil {
		*f.order = append(*f.order, "refs:"+remote.Name)
	}
	if f.refsErr != nil && (f.refsErrFor == "" || f.refsErrFor == remote.Name) {
		return nil, f.refsErr
	}
	data := f.refsContent[remote.Name]
	if data == nil {
		data = []byte("refs-bytes-" + remote.Name)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// fakePushHold records Engage/Release calls in order, and their relative
// ordering against the database/git capturers they wrap, for tests to
// assert on. errFor selects which call ("engage" or "release") fails.
type fakePushHold struct {
	calls  []string
	errFor string
	err    error

	// order, when set, also gets "engage"/"release" appended — see
	// fakeGitCapturer.order.
	order *[]string
}

func (f *fakePushHold) Engage(ctx context.Context) error {
	f.calls = append(f.calls, "engage")
	if f.order != nil {
		*f.order = append(*f.order, "engage")
	}
	if f.errFor == "engage" {
		return f.err
	}
	return nil
}

func (f *fakePushHold) Release(ctx context.Context) error {
	f.calls = append(f.calls, "release")
	if f.order != nil {
		*f.order = append(*f.order, "release")
	}
	if f.errFor == "release" {
		return f.err
	}
	return nil
}

type fakeDatabaseExporter struct {
	data []byte
	err  error
}

func (f *fakeDatabaseExporter) Snapshot(ctx context.Context) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(bytes.NewReader(f.data)), nil
}

type fakeBlobExporter struct {
	objects   []blob.Object
	content   map[string][]byte
	listErr   error
	getErr    error
	getErrFor string
}

func (f *fakeBlobExporter) List(ctx context.Context, prefix string) ([]blob.Object, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.objects, nil
}

func (f *fakeBlobExporter) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if f.getErr != nil && (f.getErrFor == "" || f.getErrFor == key) {
		return nil, f.getErr
	}
	return io.NopCloser(bytes.NewReader(f.content[key])), nil
}

type fakeKeyExporter struct {
	names         []string
	values        map[string]string
	resolveErr    error
	resolveErrFor string
}

func (f *fakeKeyExporter) Names() []string {
	return f.names
}

func (f *fakeKeyExporter) Resolve(ctx context.Context, name string) (keystore.Secret, error) {
	if f.resolveErr != nil && (f.resolveErrFor == "" || f.resolveErrFor == name) {
		return keystore.Secret{}, f.resolveErr
	}
	return keystore.NewSecret(f.values[name]), nil
}

func validParams(t *testing.T) Params {
	t.Helper()
	return Params{
		Dir:            filepath.Join(t.TempDir(), "snapshot"),
		ForgejoVersion: "11.0.2",
		Git: &fakeGitExporter{remotes: []state.Remote{
			{Name: "acme/widgets", URL: "/data/git/acme/widgets.git"},
			{Name: "acme/gadgets", URL: "/data/git/acme/gadgets.git"},
		}},
		GitCapturer: &fakeGitCapturer{},
		Database:    &fakeDatabaseExporter{data: []byte("sqlite-bytes")},
		Blobs: &fakeBlobExporter{
			objects: []blob.Object{{Key: "avatars/2.png", Size: 2}, {Key: "lfs/1", Size: 1}},
			content: map[string][]byte{"avatars/2.png": []byte("avatar-bytes"), "lfs/1": []byte("lfs-bytes")},
		},
		Keys: &fakeKeyExporter{
			names:  []string{"secret_key", "internal_token"},
			values: map[string]string{"secret_key": "sk-value", "internal_token": "it-value"},
		},
		PushHold: &fakePushHold{},
	}
}

func TestRunWritesACompleteSnapshot(t *testing.T) {
	params := validParams(t)
	job := events.NewJob()

	manifest, err := Run(context.Background(), job, params)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if manifest.ForgejoVersion != params.ForgejoVersion {
		t.Errorf("ForgejoVersion = %q, want %q", manifest.ForgejoVersion, params.ForgejoVersion)
	}
	if manifest.ChecksumAlgorithm != bundle.DefaultChecksumAlgorithm {
		t.Errorf("ChecksumAlgorithm = %q, want %q", manifest.ChecksumAlgorithm, bundle.DefaultChecksumAlgorithm)
	}
	if manifest.Timestamp.IsZero() {
		t.Error("Timestamp is zero")
	}

	// database(1) + git refs(2) + git objects(2) + blobs(2) + keys(2) = 9
	if len(manifest.Components) != 9 {
		t.Fatalf("Components = %d, want 9: %+v", len(manifest.Components), manifest.Components)
	}

	byKind := make(map[bundle.StateKind][]Component)
	for _, c := range manifest.Components {
		byKind[c.Kind] = append(byKind[c.Kind], c)

		full := filepath.Join(params.Dir, filepath.FromSlash(c.Path))
		data, err := os.ReadFile(full)
		if err != nil {
			t.Fatalf("read component file %s: %v", full, err)
		}
		if got := sha256Hex(data); got != c.Checksum {
			t.Errorf("component %s checksum = %s, want %s", c.Name, c.Checksum, got)
		}
	}
	if len(byKind[bundle.StateKindDatabase]) != 1 {
		t.Errorf("database components = %d, want 1", len(byKind[bundle.StateKindDatabase]))
	}
	if len(byKind[bundle.StateKindGit]) != 4 {
		t.Errorf("git components = %d, want 4 (2 repositories x refs+objects)", len(byKind[bundle.StateKindGit]))
	}
	if len(byKind[bundle.StateKindBlobs]) != 2 {
		t.Errorf("blob components = %d, want 2", len(byKind[bundle.StateKindBlobs]))
	}
	if len(byKind[bundle.StateKindKeys]) != 2 {
		t.Errorf("key components = %d, want 2", len(byKind[bundle.StateKindKeys]))
	}

	dbContent, err := os.ReadFile(filepath.Join(params.Dir, databaseFile))
	if err != nil {
		t.Fatalf("read db.sqlite: %v", err)
	}
	if string(dbContent) != "sqlite-bytes" {
		t.Errorf("db.sqlite content = %q, want %q", dbContent, "sqlite-bytes")
	}

	keyContent, err := os.ReadFile(filepath.Join(params.Dir, keysDir, "secret_key"))
	if err != nil {
		t.Fatalf("read keys/secret_key: %v", err)
	}
	if string(keyContent) != "sk-value" {
		t.Errorf("keys/secret_key content = %q, want %q", keyContent, "sk-value")
	}

	onDisk, err := os.ReadFile(filepath.Join(params.Dir, ManifestFile))
	if err != nil {
		t.Fatalf("read manifest file: %v", err)
	}
	var loaded Manifest
	if err := json.Unmarshal(onDisk, &loaded); err != nil {
		t.Fatalf("unmarshal manifest file: %v", err)
	}
	if loaded.ForgejoVersion != manifest.ForgejoVersion {
		t.Errorf("manifest on disk ForgejoVersion = %q, want %q", loaded.ForgejoVersion, manifest.ForgejoVersion)
	}

	assertJobSucceeded(t, job)
}

func TestRunCapturesBlobsInSortedOrder(t *testing.T) {
	params := validParams(t)
	params.Blobs = &fakeBlobExporter{
		objects: []blob.Object{{Key: "z-last"}, {Key: "a-first"}, {Key: "m-mid"}},
		content: map[string][]byte{"z-last": []byte("z"), "a-first": []byte("a"), "m-mid": []byte("m")},
	}

	manifest, err := Run(context.Background(), events.NewJob(), params)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var blobNames []string
	for _, c := range manifest.Components {
		if c.Kind == bundle.StateKindBlobs {
			blobNames = append(blobNames, c.Name)
		}
	}
	want := []string{"a-first", "m-mid", "z-last"}
	if !sort.StringsAreSorted(blobNames) || len(blobNames) != len(want) {
		t.Fatalf("blob component order = %v, want sorted %v", blobNames, want)
	}
	for i := range want {
		if blobNames[i] != want[i] {
			t.Fatalf("blob component order = %v, want %v", blobNames, want)
		}
	}
}

func TestRunCapturesStepsInOrder(t *testing.T) {
	params := validParams(t)
	job := events.NewJob()

	if _, err := Run(context.Background(), job, params); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var order []string
	for _, ev := range job.Events() {
		if ev.State == events.StateStarted {
			order = append(order, ev.Step)
		}
	}
	want := []string{StepValidate, StepPushHold, StepDatabase, StepRecordRefs, StepGit, StepBlobs, StepKeys, StepWriteManifest}
	if len(order) != len(want) {
		t.Fatalf("started steps = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("started steps = %v, want %v", order, want)
		}
	}
}

func TestRunRejectsMissingDir(t *testing.T) {
	params := validParams(t)
	params.Dir = ""
	job := events.NewJob()

	if _, err := Run(context.Background(), job, params); err == nil {
		t.Fatal("Run: want error for missing dir, got nil")
	}
	assertJobFailed(t, job)
}

func TestRunRejectsMissingForgejoVersion(t *testing.T) {
	params := validParams(t)
	params.ForgejoVersion = ""

	if _, err := Run(context.Background(), events.NewJob(), params); err == nil {
		t.Fatal("Run: want error for missing forgejo version, got nil")
	}
}

func TestRunRejectsMissingExporters(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Params)
	}{
		{"git exporter", func(p *Params) { p.Git = nil }},
		{"git capturer", func(p *Params) { p.GitCapturer = nil }},
		{"database exporter", func(p *Params) { p.Database = nil }},
		{"blob exporter", func(p *Params) { p.Blobs = nil }},
		{"key exporter", func(p *Params) { p.Keys = nil }},
		{"push hold", func(p *Params) { p.PushHold = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			params := validParams(t)
			tc.mutate(&params)
			job := events.NewJob()

			if _, err := Run(context.Background(), job, params); err == nil {
				t.Fatalf("Run: want error with no %s, got nil", tc.name)
			}
			assertJobFailed(t, job)
		})
	}
}

func TestRunPropagatesDatabaseError(t *testing.T) {
	params := validParams(t)
	params.Database = &fakeDatabaseExporter{err: errors.New("database unreachable")}
	job := events.NewJob()

	_, err := Run(context.Background(), job, params)
	if err == nil {
		t.Fatal("Run: want error, got nil")
	}
	if !strings.Contains(err.Error(), "database unreachable") {
		t.Errorf("error = %v, want it to wrap the database exporter's error", err)
	}
	assertJobFailed(t, job)
}

func TestRunPropagatesGitListError(t *testing.T) {
	params := validParams(t)
	params.Git = &fakeGitExporter{err: errors.New("cannot list repositories")}
	job := events.NewJob()

	_, err := Run(context.Background(), job, params)
	if err == nil {
		t.Fatal("Run: want error, got nil")
	}
	if !strings.Contains(err.Error(), "cannot list repositories") {
		t.Errorf("error = %v, want it to wrap the git exporter's error", err)
	}
	assertJobFailed(t, job)
}

func TestRunPropagatesGitArchiveError(t *testing.T) {
	params := validParams(t)
	params.GitCapturer = &fakeGitCapturer{err: errors.New("archive failed"), errFor: "acme/gadgets"}
	job := events.NewJob()

	_, err := Run(context.Background(), job, params)
	if err == nil {
		t.Fatal("Run: want error, got nil")
	}
	if !strings.Contains(err.Error(), "archive failed") || !strings.Contains(err.Error(), "acme/gadgets") {
		t.Errorf("error = %v, want it to name acme/gadgets and wrap the archive error", err)
	}
	assertJobFailed(t, job)
}

func TestRunHoldsPushesOnlyAcrossDatabaseAndRefRecording(t *testing.T) {
	var order []string
	params := validParams(t)
	params.PushHold = &fakePushHold{order: &order}
	params.GitCapturer = &fakeGitCapturer{order: &order}
	job := events.NewJob()

	if _, err := Run(context.Background(), job, params); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(order) == 0 || order[0] != "engage" {
		t.Fatalf("order = %v, want it to start with engage", order)
	}
	releaseIdx := -1
	for i, step := range order {
		if step == "release" {
			releaseIdx = i
			break
		}
	}
	if releaseIdx == -1 {
		t.Fatalf("order = %v, push hold was never released", order)
	}
	for i, step := range order {
		if strings.HasPrefix(step, "archive:") && i < releaseIdx {
			t.Errorf("order = %v: %q (object tar) ran before the push hold released", order, step)
		}
		if strings.HasPrefix(step, "refs:") && i > releaseIdx {
			t.Errorf("order = %v: %q (ref recording) ran after the push hold released", order, step)
		}
	}
}

func TestRunReleasesPushHoldOnDatabaseError(t *testing.T) {
	params := validParams(t)
	hold := &fakePushHold{}
	params.PushHold = hold
	params.Database = &fakeDatabaseExporter{err: errors.New("database unreachable")}
	job := events.NewJob()

	if _, err := Run(context.Background(), job, params); err == nil {
		t.Fatal("Run: want error, got nil")
	}
	if len(hold.calls) != 2 || hold.calls[0] != "engage" || hold.calls[1] != "release" {
		t.Fatalf("push hold calls = %v, want [engage release]", hold.calls)
	}
}

func TestRunPropagatesPushHoldEngageError(t *testing.T) {
	params := validParams(t)
	params.PushHold = &fakePushHold{errFor: "engage", err: errors.New("caddy reload failed")}
	job := events.NewJob()

	_, err := Run(context.Background(), job, params)
	if err == nil {
		t.Fatal("Run: want error, got nil")
	}
	if !strings.Contains(err.Error(), "caddy reload failed") {
		t.Errorf("error = %v, want it to wrap the engage error", err)
	}
	assertJobFailed(t, job)
}

func TestRunPropagatesPushHoldReleaseError(t *testing.T) {
	params := validParams(t)
	params.PushHold = &fakePushHold{errFor: "release", err: errors.New("caddy reload failed")}
	job := events.NewJob()

	_, err := Run(context.Background(), job, params)
	if err == nil {
		t.Fatal("Run: want error, got nil")
	}
	if !strings.Contains(err.Error(), "caddy reload failed") {
		t.Errorf("error = %v, want it to wrap the release error", err)
	}
	assertJobFailed(t, job)
}

func TestRunPropagatesRefsError(t *testing.T) {
	params := validParams(t)
	params.GitCapturer = &fakeGitCapturer{refsErr: errors.New("refs capture failed"), refsErrFor: "acme/gadgets"}
	job := events.NewJob()

	_, err := Run(context.Background(), job, params)
	if err == nil {
		t.Fatal("Run: want error, got nil")
	}
	if !strings.Contains(err.Error(), "refs capture failed") || !strings.Contains(err.Error(), "acme/gadgets") {
		t.Errorf("error = %v, want it to name acme/gadgets and wrap the refs error", err)
	}
	assertJobFailed(t, job)
}

func TestRunReleasesPushHoldWhenCeilingExpires(t *testing.T) {
	params := validParams(t)
	hold := &fakePushHold{}
	params.PushHold = hold
	params.PushHoldCeiling = time.Nanosecond
	params.Database = &blockingDatabaseExporter{}
	job := events.NewJob()

	if _, err := Run(context.Background(), job, params); err == nil {
		t.Fatal("Run: want error when the push hold ceiling expires, got nil")
	}
	if len(hold.calls) != 2 || hold.calls[0] != "engage" || hold.calls[1] != "release" {
		t.Fatalf("push hold calls = %v, want [engage release]", hold.calls)
	}
}

// blockingDatabaseExporter blocks until its context is canceled, standing
// in for a wedged capture that only a push-hold ceiling can bound.
type blockingDatabaseExporter struct{}

func (blockingDatabaseExporter) Snapshot(ctx context.Context) (io.ReadCloser, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRunPropagatesBlobListError(t *testing.T) {
	params := validParams(t)
	params.Blobs = &fakeBlobExporter{listErr: errors.New("blob store unreachable")}
	job := events.NewJob()

	_, err := Run(context.Background(), job, params)
	if err == nil {
		t.Fatal("Run: want error, got nil")
	}
	if !strings.Contains(err.Error(), "blob store unreachable") {
		t.Errorf("error = %v, want it to wrap the blob exporter's error", err)
	}
	assertJobFailed(t, job)
}

func TestRunPropagatesBlobGetError(t *testing.T) {
	params := validParams(t)
	blobs := params.Blobs.(*fakeBlobExporter)
	blobs.getErr = errors.New("object missing")
	blobs.getErrFor = "lfs/1"
	job := events.NewJob()

	_, err := Run(context.Background(), job, params)
	if err == nil {
		t.Fatal("Run: want error, got nil")
	}
	if !strings.Contains(err.Error(), "object missing") || !strings.Contains(err.Error(), "lfs/1") {
		t.Errorf("error = %v, want it to name lfs/1 and wrap the get error", err)
	}
	assertJobFailed(t, job)
}

func TestRunPropagatesKeyResolveError(t *testing.T) {
	params := validParams(t)
	keys := params.Keys.(*fakeKeyExporter)
	keys.resolveErr = errors.New("keystore unreachable")
	keys.resolveErrFor = "internal_token"
	job := events.NewJob()

	_, err := Run(context.Background(), job, params)
	if err == nil {
		t.Fatal("Run: want error, got nil")
	}
	if !strings.Contains(err.Error(), "keystore unreachable") || !strings.Contains(err.Error(), "internal_token") {
		t.Errorf("error = %v, want it to name internal_token and wrap the resolve error", err)
	}
	if strings.Contains(err.Error(), "it-value") {
		t.Errorf("error = %v, must never carry a secret value (KEY-003)", err)
	}
	assertJobFailed(t, job)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func assertJobSucceeded(t *testing.T, job *events.Job) {
	t.Helper()
	if !job.Done() {
		t.Fatal("job did not reach a terminal event")
	}
	evs := job.Events()
	last := evs[len(evs)-1]
	if last.Step != "" || last.State != events.StateSucceeded {
		t.Errorf("last event = %+v, want a job-terminal succeeded event", last)
	}
	for _, ev := range evs {
		if ev.State == events.StateFailed {
			t.Errorf("unexpected failed event: %+v", ev)
		}
	}
}

func assertJobFailed(t *testing.T, job *events.Job) {
	t.Helper()
	if !job.Done() {
		t.Fatal("job did not reach a terminal event")
	}
	evs := job.Events()
	last := evs[len(evs)-1]
	if last.Step != "" || last.State != events.StateFailed {
		t.Errorf("last event = %+v, want a job-terminal failed event", last)
	}
}
