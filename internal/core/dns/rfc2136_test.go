package dns

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// newTestRFC2136Driver builds a driver whose "nsupdate" command is `tee`,
// writing whatever script it's given (via stdin) to outFile, so tests can
// inspect the exact update transaction without a real DNS server.
func newTestRFC2136Driver(t *testing.T, cfg RFC2136Config) (*RFC2136Driver, string) {
	t.Helper()
	outFile := t.TempDir() + "/nsupdate.script"
	cfg.Command = "tee"
	cfg.Args = []string{outFile}

	d, err := NewRFC2136(cfg)
	if err != nil {
		t.Fatalf("NewRFC2136: %v", err)
	}
	return d, outFile
}

func baseRFC2136Config() RFC2136Config {
	return RFC2136Config{
		Server:    "ns1.example.com:53",
		Zone:      "example.com",
		KeyName:   "farrier-key",
		KeySecret: "c2VjcmV0",
	}
}

func TestRFC2136SetWritesUpdateScript(t *testing.T) {
	d, outFile := newTestRFC2136Driver(t, baseRFC2136Config())

	if err := d.Set(context.Background(), "app.example.com", "203.0.113.10", 60*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}

	script, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	got := string(script)

	for _, want := range []string{
		"key hmac-sha256:farrier-key c2VjcmV0\n",
		"server ns1.example.com 53\n",
		"zone example.com\n",
		"update delete app.example.com\n",
		"update add app.example.com 60 A 203.0.113.10\n",
		"send\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("script %q does not contain %q", got, want)
		}
	}
}

func TestRFC2136SetHostnameValueUsesCNAME(t *testing.T) {
	d, outFile := newTestRFC2136Driver(t, baseRFC2136Config())

	if err := d.Set(context.Background(), "app.example.com", "standby.example.com", 60*time.Second); err != nil {
		t.Fatalf("Set: %v", err)
	}

	script, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	if !strings.Contains(string(script), "update add app.example.com 60 CNAME standby.example.com\n") {
		t.Fatalf("script %q does not contain expected CNAME update", script)
	}
}

func TestRFC2136DeleteWritesDeleteScript(t *testing.T) {
	d, outFile := newTestRFC2136Driver(t, baseRFC2136Config())

	if err := d.Delete(context.Background(), "app.example.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	script, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	got := string(script)
	if !strings.Contains(got, "update delete app.example.com\n") || !strings.Contains(got, "send\n") {
		t.Fatalf("script %q does not look like a delete transaction", got)
	}
	if strings.Contains(got, "update add") {
		t.Fatalf("script %q unexpectedly contains an add", got)
	}
}

func TestRFC2136ServerWithoutPortDefaultsTo53(t *testing.T) {
	cfg := baseRFC2136Config()
	cfg.Server = "ns1.example.com"
	d, outFile := newTestRFC2136Driver(t, cfg)

	if err := d.Delete(context.Background(), "app.example.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	script, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	if !strings.Contains(string(script), "server ns1.example.com 53\n") {
		t.Fatalf("script %q does not default to port 53", script)
	}
}

func TestRFC2136CustomAlgorithm(t *testing.T) {
	cfg := baseRFC2136Config()
	cfg.Algorithm = "hmac-sha512"
	d, outFile := newTestRFC2136Driver(t, cfg)

	if err := d.Delete(context.Background(), "app.example.com"); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	script, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	if !strings.Contains(string(script), "key hmac-sha512:farrier-key c2VjcmV0\n") {
		t.Fatalf("script %q does not use the configured algorithm", script)
	}
}

func TestRFC2136CommandFailurePropagatesStderr(t *testing.T) {
	cfg := baseRFC2136Config()
	cfg.Command = "sh"
	cfg.Args = []string{"-c", "cat >/dev/null; echo update failed: NOTAUTH >&2; exit 1"}

	d, err := NewRFC2136(cfg)
	if err != nil {
		t.Fatalf("NewRFC2136: %v", err)
	}

	err = d.Delete(context.Background(), "app.example.com")
	if err == nil || !strings.Contains(err.Error(), "NOTAUTH") {
		t.Fatalf("Delete error = %v, want it to mention %q", err, "NOTAUTH")
	}
}

func TestNewRFC2136RequiresFields(t *testing.T) {
	cases := []func(*RFC2136Config){
		func(c *RFC2136Config) { c.Server = "" },
		func(c *RFC2136Config) { c.Zone = "" },
		func(c *RFC2136Config) { c.KeyName = "" },
		func(c *RFC2136Config) { c.KeySecret = "" },
	}
	for _, mutate := range cases {
		cfg := baseRFC2136Config()
		mutate(&cfg)
		if _, err := NewRFC2136(cfg); err == nil {
			t.Fatalf("NewRFC2136(%+v): want error, got nil", cfg)
		}
	}
}

func TestNewRFC2136DefaultsAlgorithmAndCommand(t *testing.T) {
	d, err := NewRFC2136(baseRFC2136Config())
	if err != nil {
		t.Fatalf("NewRFC2136: %v", err)
	}
	if d.algorithm != "hmac-sha256" {
		t.Fatalf("algorithm = %q, want hmac-sha256", d.algorithm)
	}
	if d.command != "nsupdate" {
		t.Fatalf("command = %q, want nsupdate", d.command)
	}
}

func TestSplitServerAddr(t *testing.T) {
	if host, port := splitServerAddr("ns1.example.com:53"); host != "ns1.example.com" || port != "53" {
		t.Fatalf("splitServerAddr(host:port) = (%q, %q), want (ns1.example.com, 53)", host, port)
	}
	if host, port := splitServerAddr("ns1.example.com"); host != "ns1.example.com" || port != "53" {
		t.Fatalf("splitServerAddr(host) = (%q, %q), want (ns1.example.com, 53)", host, port)
	}
}

func TestRFC2136SetValidatesArgs(t *testing.T) {
	d, _ := newTestRFC2136Driver(t, baseRFC2136Config())

	if err := d.Set(context.Background(), "", "203.0.113.10", 60*time.Second); err == nil {
		t.Fatal("Set with empty record: want error, got nil")
	}
	if err := d.Delete(context.Background(), ""); err == nil {
		t.Fatal("Delete with empty record: want error, got nil")
	}
}
