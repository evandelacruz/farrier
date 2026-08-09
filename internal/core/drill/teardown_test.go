package drill

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/deploy"
	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
)

// assertScratchTargetClean asserts DRIL-003's observable outcome on the
// host: the drilled instance's Compose project was stopped and removed, and
// the drill's remote directory — the git repositories, the database, the
// rendered app.ini, the runner secret, the SSH host key — is gone with it.
func assertScratchTargetClean(t *testing.T, host *fakeHost, remoteDir string) {
	t.Helper()
	if got := host.commandsContaining("docker compose down"); len(got) != 1 {
		t.Errorf("want exactly one compose down on the scratch target, got %v", got)
	}
	if got := host.commandsContaining("rm -rf '" + remoteDir + "'"); len(got) != 1 {
		t.Errorf("want exactly one removal of %s, got %v", remoteDir, got)
	}
}

// TestDrillTearsDownAfterASuccessfulDrill is DRIL-003's happy path: the
// rehearsal passed and the scratch target is left as it was found.
func TestDrillTearsDownAfterASuccessfulDrill(t *testing.T) {
	f := newFixture(t)
	host := f.host()

	job := events.NewJob()
	report, err := Drill(context.Background(), job, f.opts)
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}
	if !report.Clean() {
		t.Fatalf("report.Clean() = false, teardown = %+v", report.Teardown)
	}
	assertScratchTargetClean(t, host, f.opts.RemoteDir)

	// Teardown is last: a drill that tore the stack down before waiting on
	// it would be rehearsing nothing.
	converge := host.commandsContaining("docker compose up -d --remove-orphans")
	if len(converge) != 1 {
		t.Fatalf("want exactly one converge command, got %v", converge)
	}
	var sawConverge bool
	for _, c := range host.commands {
		if strings.Contains(c, "docker compose up -d --remove-orphans") {
			sawConverge = true
		}
		if strings.Contains(c, "docker compose down") && !sawConverge {
			t.Fatalf("the scratch target was torn down before it was converged: %v", host.commands)
		}
	}

	if state := stepState(job, StepTeardown); state != events.StateSucceeded {
		t.Errorf("teardown step reported %q, want %q", state, events.StateSucceeded)
	}
}

// TestDrillTearsDownAfterAFailedStep is the case DRIL-003 exists for: a
// drill that failed partway through booting the stack must not leave the
// half-booted instance and production's state behind on the scratch target.
func TestDrillTearsDownAfterAFailedStep(t *testing.T) {
	// One case per phase of the drill: before anything reaches the host,
	// after state is on the host but before the stack is up, and with the
	// stack already running.
	tests := []struct {
		name   string
		failOn string
		want   string
	}{
		{name: "before the host is touched", failOn: "docker version", want: deploy.StepCheckHost},
		{name: "with state already placed", failOn: "docker compose up -d", want: deploy.StepConverge},
		{name: "with the stack running", failOn: "admin user create", want: forge.StepAdminBootstrap},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			host := f.host()
			host.failOn = tt.failOn

			job := events.NewJob()
			report, err := Drill(context.Background(), job, f.opts)
			if err == nil {
				t.Fatal("Drill: want an error")
			}
			if report.Failure == nil || report.Failure.Step != tt.want {
				t.Fatalf("report.Failure = %+v, want the drill to fail at %s", report.Failure, tt.want)
			}
			if !report.Clean() {
				t.Fatalf("report.Clean() = false after a failed drill, teardown = %+v", report.Teardown)
			}
			assertScratchTargetClean(t, host, f.opts.RemoteDir)
		})
	}
}

// TestDrillTearsDownAfterAnEarlyFailure covers the failure that happens
// before the host is reached at all: the snapshot could not even be
// resolved. There is nothing on the scratch target to remove, and the
// teardown must still run and still report success rather than inventing a
// failure out of an empty host.
func TestDrillTearsDownAfterAnEarlyFailure(t *testing.T) {
	f := newFixture(t)
	f.opts.Source = mustLocalBlob(t, t.TempDir()) // no snapshots at all
	host := f.host()

	job := events.NewJob()
	report, err := Drill(context.Background(), job, f.opts)
	if err == nil {
		t.Fatal("Drill: want an error")
	}
	if report.Failure == nil || report.Failure.Step != StepResolveSnapshot {
		t.Fatalf("report.Failure = %+v, want a failure at %s", report.Failure, StepResolveSnapshot)
	}
	if !report.Clean() {
		t.Fatalf("report.Clean() = false, teardown = %+v", report.Teardown)
	}
	assertScratchTargetClean(t, host, f.opts.RemoteDir)
}

// TestDrillTearsDownOnACanceledContext pins the reason teardown runs on a
// detached context: cancellation is the path that would otherwise leave the
// most behind, since every command the drill was midway through fails at
// once and nothing cleans up after them.
func TestDrillTearsDownOnACanceledContext(t *testing.T) {
	f := newFixture(t)
	host := f.host()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	job := events.NewJob()
	report, err := Drill(ctx, job, f.opts)
	if err == nil {
		t.Fatal("Drill: want an error on a canceled context")
	}
	if report.Failure == nil {
		t.Fatal("report.Failure = nil, want the canceled drill to name a failing step")
	}
	if !report.Clean() {
		t.Fatalf("report.Clean() = false on a canceled drill, teardown = %+v", report.Teardown)
	}
	assertScratchTargetClean(t, host, f.opts.RemoteDir)
}

// TestDrillTearsDownWhileAPanicUnwinds pins the last exit path: a bug
// anywhere under Drill still leaves the scratch target clean, because
// teardown is deferred rather than called at the end. The panic itself is
// deliberately not swallowed — the caller gets its stack trace.
func TestDrillTearsDownWhileAPanicUnwinds(t *testing.T) {
	f := newFixture(t)
	host := f.host()
	host.panicOn = "docker compose up -d"

	func() {
		defer func() {
			if recover() == nil {
				t.Error("Drill: want the panic to propagate to the caller")
			}
		}()
		//nolint:errcheck // the call panics; neither result is reached.
		Drill(context.Background(), events.NewJob(), f.opts)
	}()

	assertScratchTargetClean(t, host, f.opts.RemoteDir)
}

// TestDrillReportsAFailedTeardown is the other half of DRIL-003: a scratch
// target that could not be cleaned is the operator's problem, and a drill
// whose rehearsal passed must not report success over it.
func TestDrillReportsAFailedTeardown(t *testing.T) {
	f := newFixture(t)
	f.host().failOn = "docker compose down"

	job := events.NewJob()
	report, err := Drill(context.Background(), job, f.opts)
	if err == nil {
		t.Fatal("Drill: want an error when the scratch target could not be cleaned")
	}

	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("Drill returned %T, want a *Failure", err)
	}
	if failure.Step != StepTeardown {
		t.Errorf("failure.Step = %q, want %q", failure.Step, StepTeardown)
	}
	if report.Failure != nil {
		t.Errorf("report.Failure = %+v, want nil: the rehearsal itself passed", report.Failure)
	}
	if report.Teardown == nil || report.Teardown.Step != StepTeardown {
		t.Fatalf("report.Teardown = %+v, want a teardown failure", report.Teardown)
	}
	if report.Succeeded() {
		t.Error("report.Succeeded() = true with the scratch target left dirty")
	}
	if state := stepState(job, StepTeardown); state != events.StateFailed {
		t.Errorf("teardown step reported %q, want %q", state, events.StateFailed)
	}
	if !job.Done() {
		t.Error("Drill did not end the job")
	}
}

// TestDrillReportsBothFailures pins that neither fact is lost when the
// rehearsal and the teardown both fail: the returned error names the
// rehearsal's step, the report carries both, and the job's terminal event
// says the scratch target is dirty — which is the part an operator has to
// act on.
func TestDrillReportsBothFailures(t *testing.T) {
	f := newFixture(t)
	// Fails deploy.Up's very first command and the teardown's compose call
	// alike: both contain "docker".
	f.host().failOn = "docker"

	job := events.NewJob()
	report, err := Drill(context.Background(), job, f.opts)
	if err == nil {
		t.Fatal("Drill: want an error")
	}

	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("Drill returned %T, want a *Failure", err)
	}
	if failure.Step != deploy.StepCheckHost {
		t.Errorf("failure.Step = %q, want the rehearsal's own step %q", failure.Step, deploy.StepCheckHost)
	}
	if report.Failure == nil || report.Teardown == nil {
		t.Fatalf("report.Failure = %+v, report.Teardown = %+v, want both", report.Failure, report.Teardown)
	}

	terminal := job.Events()[len(job.Events())-1]
	if terminal.State != events.StateFailed {
		t.Fatalf("terminal event state = %q, want %q", terminal.State, events.StateFailed)
	}
	if !strings.Contains(terminal.Detail, deploy.StepCheckHost) {
		t.Errorf("terminal detail %q does not name the failing rehearsal step", terminal.Detail)
	}
	if !strings.Contains(terminal.Detail, "dirty") {
		t.Errorf("terminal detail %q does not say the scratch target was left dirty", terminal.Detail)
	}
}

// TestTeardownIsBounded pins that teardown cannot hang forever just because
// it runs on a context the caller can no longer cancel.
func TestTeardownIsBounded(t *testing.T) {
	restore := teardownTimeout
	teardownTimeout = time.Millisecond
	defer func() { teardownTimeout = restore }()

	f := newFixture(t)
	f.opts.Host = blockingHost{fakeHost: newFakeHost()}

	failure := teardown(context.Background(), events.NewJob(), f.opts)
	if failure == nil {
		t.Fatal("teardown: want a failure once its own deadline passes")
	}
	if failure.Step != StepTeardown {
		t.Errorf("failure.Step = %q, want %q", failure.Step, StepTeardown)
	}
}

// blockingHost stands in for a host whose commands never return on their
// own: Output blocks until the context it was given is done.
type blockingHost struct {
	*fakeHost
}

func (b blockingHost) Output(ctx context.Context, command string) ([]byte, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// stepState returns the last state reported for step on job, or "" if the
// step never reported.
func stepState(job *events.Job, step string) events.State {
	var state events.State
	for _, ev := range job.Events() {
		if ev.Step == step {
			state = ev.State
		}
	}
	return state
}
