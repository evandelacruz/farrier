package initialize

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
	"github.com/evandelacruz/farrier/internal/core/keystore"
)

// allKeyNames is every piece of key material init generates, in the order
// it stores them. Kept beside the reporting tests because INIT-006's
// promise is per-piece: a report that quietly omitted one would still look
// like a report.
var allKeyNames = []string{
	forge.KeySecretKey,
	forge.KeyInternalToken,
	forge.KeyLFSJWTSecret,
	forge.KeyRunnerSecret,
	KeyTLSCertificate,
	KeyTLSPrivateKey,
	KeySSHHostKey,
	KeySSHHostKeyPublic,
	KeyAgeBackupKey,
}

// runWithFileKeystore runs init against a file keystore in a fresh
// directory, returning the job it drove and that directory.
func runWithFileKeystore(t *testing.T) (*events.Job, string) {
	t.Helper()
	keysDir := t.TempDir()
	params := validParams(t, &fakeResolver{})
	params.Keystore = bundle.DriverRef{Driver: "file", Config: map[string]any{"path": keysDir}}
	job := events.NewJob()
	if _, err := Run(context.Background(), job, params); err != nil {
		t.Fatalf("Run: %v", err)
	}
	return job, keysDir
}

// reportDetails returns the detail of every event the report step emitted.
func reportDetails(job *events.Job) []string {
	var details []string
	for _, ev := range job.Events() {
		if ev.Step == StepReportKeys && ev.State == events.StateSucceeded {
			details = append(details, ev.Detail)
		}
	}
	return details
}

func TestRunReportsWhereEachPieceOfKeyMaterialWasStored(t *testing.T) {
	job, keysDir := runWithFileKeystore(t)
	details := reportDetails(job)

	for _, name := range allKeyNames {
		var found string
		for _, detail := range details {
			if strings.HasPrefix(detail, name+" ") {
				found = detail
				break
			}
		}
		if found == "" {
			t.Errorf("no report event for %s; events = %q", name, details)
			continue
		}
		if !strings.Contains(found, "file keystore driver") {
			t.Errorf("report for %s = %q, want it to name the configured driver", name, found)
		}
		if want := filepath.Join(keysDir, name); !strings.Contains(found, want) {
			t.Errorf("report for %s = %q, want it to name the target %s", name, found, want)
		}
	}
}

// The age backup key is the only unrecoverable loss in the system, so
// init has to say so in the same words docs/security.md and
// docs/operating.md use.
func TestRunWarnsThatTheAgeBackupKeyIsUnrecoverable(t *testing.T) {
	job, _ := runWithFileKeystore(t)

	var warning string
	for _, detail := range reportDetails(job) {
		if strings.Contains(detail, "unrecoverable") {
			warning = detail
			break
		}
	}
	if warning == "" {
		t.Fatalf("no event stated the age backup key is unrecoverable; events = %q", reportDetails(job))
	}
	for _, want := range []string{KeyAgeBackupKey, "age-encrypted", "permanently unreadable"} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning = %q, want it to mention %q", warning, want)
		}
	}
}

// KEY-003 on the reporting path: naming the destination is the point, the
// value never is. Every stored secret is checked against every event
// detail, not just the report step's, so a leak anywhere in init's stream
// fails here.
func TestRunReportsDestinationsWithoutRevealingKeyMaterial(t *testing.T) {
	job, keysDir := runWithFileKeystore(t)

	driver := keystore.FileDriver{Path: keysDir}
	for _, name := range allKeyNames {
		secret, err := driver.Resolve(context.Background(), name)
		if err != nil {
			t.Fatalf("resolve %s: %v", name, err)
		}
		value := strings.TrimSpace(secret.Reveal())
		if value == "" {
			t.Fatalf("%s resolved empty, cannot check it does not leak", name)
		}
		for _, ev := range job.Events() {
			if strings.Contains(ev.Detail, value) {
				t.Errorf("event %q leaked the value of %s", ev.Detail, name)
			}
		}
	}
}

// The report runs the moment key material is safely stored, so a later
// failure — an unwritable bundle directory — is never the reason an
// operator does not learn where the age key went. Image resolution used to
// be the "later failure" this guarded against; it now runs before any key
// material exists, which is a stronger version of the same guarantee, and
// the bundle write is what is left after the report.
func TestRunReportsKeyMaterialBeforeWritingTheBundle(t *testing.T) {
	job, _ := runWithFileKeystore(t)

	report, write := -1, -1
	for i, ev := range job.Events() {
		if ev.Step == StepReportKeys && ev.State == events.StateStarted {
			report = i
		}
		if ev.Step == StepWrite && ev.State == events.StateStarted && write == -1 {
			write = i
		}
	}
	if report == -1 || write == -1 {
		t.Fatalf("did not see both steps: report=%d write=%d", report, write)
	}
	if report >= write {
		t.Errorf("report started at event %d, write at %d, want the report first", report, write)
	}
}

// INIT-005 × INIT-006: a nameless bundle issues no certificate, so the
// report must say so by omission rather than naming a destination for two
// pieces of key material that were never generated. An operator reading
// "TLS certificate → /keys/tls.crt" would go looking for a file that does
// not exist, and would believe the instance is serving HTTPS when it is
// not. Everything else is reported exactly as it is for a named bundle,
// including the age key warning — a nameless instance takes real backups.
func TestRunWithNoDomainReportsEveryKeyButTLS(t *testing.T) {
	keysDir := t.TempDir()
	params := namelessParams(t, &fakeResolver{})
	params.Keystore = bundle.DriverRef{Driver: "file", Config: map[string]any{"path": keysDir}}
	job := events.NewJob()
	if _, err := Run(context.Background(), job, params); err != nil {
		t.Fatalf("Run: %v", err)
	}
	details := reportDetails(job)

	for _, name := range allKeyNames {
		var found bool
		for _, detail := range details {
			if strings.HasPrefix(detail, name+" ") {
				found = true
				break
			}
		}
		tls := name == KeyTLSCertificate || name == KeyTLSPrivateKey
		if tls && found {
			t.Errorf("report claims %s was stored, but a nameless bundle issues no certificate; events = %q", name, details)
		}
		if !tls && !found {
			t.Errorf("no report event for %s; events = %q", name, details)
		}
	}
	if !strings.Contains(details[len(details)-1], "unrecoverable") {
		t.Errorf("last report event = %q, want the age backup key warning last", details[len(details)-1])
	}
}

// describingDriver stands in for a driver whose storage farrier can name.
type describingDriver struct {
	stubKeystore
	target string
}

func (d describingDriver) DescribeTarget(string) string { return d.target }

// stubKeystore satisfies keystore.Driver and nothing else — the shape of
// an out-of-tree driver that keeps key material somewhere farrier has no
// way to name.
type stubKeystore struct{}

func (stubKeystore) Resolve(context.Context, string) (keystore.Secret, error) {
	return keystore.Secret{}, keystore.ErrNotFound
}

func TestDescribeLocationNamesTheDriverAloneWhenItCannotSay(t *testing.T) {
	got := describeLocation("vault", stubKeystore{}, KeyAgeBackupKey)

	if !strings.Contains(got, "vault keystore driver") {
		t.Errorf("location = %q, want it to name the configured driver", got)
	}
	if !strings.Contains(got, "does not report") {
		t.Errorf("location = %q, want it to say the driver does not report a target", got)
	}
}

func TestDescribeLocationNamesTheTargetWhenTheDriverSays(t *testing.T) {
	got := describeLocation("vault", describingDriver{target: "op://farrier/age"}, KeyAgeBackupKey)

	if !strings.Contains(got, "vault keystore driver") {
		t.Errorf("location = %q, want it to name the driver", got)
	}
	if !strings.Contains(got, "op://farrier/age") {
		t.Errorf("location = %q, want it to name the target the driver reported", got)
	}
}

// A driver farrier cannot get a location out of still gets a full report:
// every key named, the driver named, and the age warning intact. Reporting
// less would be reporting nothing useful in exactly the setup an operator
// most needs to see (a secret manager they configured themselves).
func TestReportKeyMaterialCoversEveryKeyForAnUndescribableDriver(t *testing.T) {
	job := events.NewJob()
	material := make(map[string]keystore.Secret, len(allKeyNames))
	for _, name := range allKeyNames {
		material[name] = keystore.NewSecret("value-of-" + name)
	}

	reportKeyMaterial(job, "vault", stubKeystore{}, material, nil)

	details := reportDetails(job)
	if len(details) != len(allKeyNames)+1 {
		t.Fatalf("got %d report events, want one per key plus the age warning; events = %q", len(details), details)
	}
	for _, name := range allKeyNames {
		var found bool
		for _, detail := range details {
			if strings.HasPrefix(detail, name+" ") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no report event for %s; events = %q", name, details)
		}
	}
	if !strings.Contains(details[len(details)-1], "unrecoverable") {
		t.Errorf("last report event = %q, want the age backup key warning last", details[len(details)-1])
	}
}
