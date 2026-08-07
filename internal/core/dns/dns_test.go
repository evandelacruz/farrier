package dns

import (
	"context"
	"testing"
	"time"
)

func TestRecordType(t *testing.T) {
	cases := map[string]string{
		"203.0.113.10":             "A",
		"2001:db8::1":              "AAAA",
		"standby.example.com":      "CNAME",
		"another-host.example.org": "CNAME",
	}
	for value, want := range cases {
		if got := recordType(value); got != want {
			t.Errorf("recordType(%q) = %q, want %q", value, got, want)
		}
	}
}

// fakeDriver records the arguments its last Set call was made with, so
// tests can assert on the ttl a caller actually sent.
type fakeDriver struct {
	record, value string
	ttl           time.Duration
	setErr        error
}

func (f *fakeDriver) Set(ctx context.Context, record, value string, ttl time.Duration) error {
	f.record, f.value, f.ttl = record, value, ttl
	return f.setErr
}

func (f *fakeDriver) Delete(ctx context.Context, record string) error {
	return nil
}

func TestSetBundleRecordUsesBundleTTL(t *testing.T) {
	if BundleTTL != 60*time.Second {
		t.Fatalf("BundleTTL = %v, want 60s", BundleTTL)
	}

	d := &fakeDriver{}
	if err := SetBundleRecord(context.Background(), d, "app.example.com", "203.0.113.10"); err != nil {
		t.Fatalf("SetBundleRecord: %v", err)
	}
	if d.record != "app.example.com" || d.value != "203.0.113.10" {
		t.Errorf("Set called with (%q, %q), want (%q, %q)", d.record, d.value, "app.example.com", "203.0.113.10")
	}
	if d.ttl != BundleTTL {
		t.Errorf("Set called with ttl %v, want %v", d.ttl, BundleTTL)
	}
}

func TestSetBundleRecordPropagatesDriverError(t *testing.T) {
	wantErr := errTest("driver unavailable")
	d := &fakeDriver{setErr: wantErr}
	if err := SetBundleRecord(context.Background(), d, "app.example.com", "203.0.113.10"); err != wantErr {
		t.Errorf("SetBundleRecord error = %v, want %v", err, wantErr)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
