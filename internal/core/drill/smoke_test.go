package drill

import (
	"context"
	"strings"
	"testing"

	"github.com/evandelacruz/farrier/internal/core/events"
	"github.com/evandelacruz/farrier/internal/core/forge"
)

// smokeCommands returns every command the drill ran that is the smoke CI
// job — the one `docker compose exec` that hands a script to the forgejo
// container.
func smokeCommands(host *fakeHost) []string {
	return host.commandsContaining("docker compose exec -T -u git forgejo sh -ec")
}

// TestDrillRunsASmokeCIJob is the clause of DRIL-001 the rest of this
// package already met: the drill does not stop at a booted stack, it makes
// the drilled instance actually run CI.
func TestDrillRunsASmokeCIJob(t *testing.T) {
	f := newFixture(t)
	host := f.host()

	job := events.NewJob()
	report, err := Drill(context.Background(), job, f.opts)
	if err != nil {
		t.Fatalf("Drill: %v", err)
	}

	commands := smokeCommands(host)
	if len(commands) != 1 {
		t.Fatalf("want exactly one smoke CI command, got %d: %v", len(commands), commands)
	}
	for _, want := range []string{
		"/user/repos",
		"contents/.forgejo/workflows/smoke.yml",
		"commits/main/status",
	} {
		if !strings.Contains(commands[0], want) {
			t.Errorf("smoke command missing %q:\n%s", want, commands[0])
		}
	}

	if report.SmokeRepository == "" {
		t.Fatal("report names no scratch repository for the smoke job")
	}
	// The report names the repository as owner/name, and the smoke script
	// writes those two on separate lines — so match each against its own
	// line. The owner is the forge's admin account, read from the forge
	// package rather than written out here: a literal would go stale the
	// moment that account is renamed, and go stale silently, since a wrong
	// prefix just leaves the rest of the assertion checking a string the
	// script never emits.
	admin, err := forge.NewAdminAccount("forge.example.com")
	if err != nil {
		t.Fatalf("NewAdminAccount: %v", err)
	}
	owner, name, ok := strings.Cut(report.SmokeRepository, "/")
	if !ok {
		t.Fatalf("report names repository %q, want owner/name form", report.SmokeRepository)
	}
	if owner != admin.Username {
		t.Errorf("report names repository %q, owned by %q — want the admin account %q", report.SmokeRepository, owner, admin.Username)
	}
	if !strings.Contains(commands[0], "owner="+owner) {
		t.Errorf("smoke command never sets owner=%s:\n%s", owner, commands[0])
	}
	if !strings.Contains(commands[0], "repo="+name) {
		t.Errorf("report names repository %q, which the smoke command never mentions:\n%s", report.SmokeRepository, commands[0])
	}

	started, terminal := stepOutcomes(job)
	if started[forge.StepSmokeCI] != 1 || terminal[forge.StepSmokeCI] != 1 {
		t.Errorf("step %s started %d time(s) and ended %d time(s), want 1 and 1",
			forge.StepSmokeCI, started[forge.StepSmokeCI], terminal[forge.StepSmokeCI])
	}
}

// The smoke job needs a booted stack to dispatch anything to, so it runs
// after the converge and the readiness waits — never beside them.
func TestDrillRunsTheSmokeJobOnlyOnceTheStackIsUp(t *testing.T) {
	f := newFixture(t)
	host := f.host()

	if _, err := Drill(context.Background(), events.NewJob(), f.opts); err != nil {
		t.Fatalf("Drill: %v", err)
	}

	host.mu.Lock()
	defer host.mu.Unlock()
	converged, smoked := -1, -1
	for i, command := range host.commands {
		switch {
		case strings.Contains(command, "docker compose up -d --remove-orphans"):
			converged = i
		case strings.Contains(command, "docker compose exec -T -u git forgejo sh -ec"):
			smoked = i
		}
	}
	if converged < 0 || smoked < 0 {
		t.Fatalf("drill did not both converge and smoke test: %v", host.commands)
	}
	if smoked < converged {
		t.Errorf("smoke job ran before the stack was converged: %v", host.commands)
	}
}

// DRIL-001's "the specific failing step" covers the smoke job like every
// other step: a drill whose restore was perfect and whose CI does not run
// is a failed drill that says so.
func TestDrillReportsAFailingSmokeJob(t *testing.T) {
	f := newFixture(t)
	f.host().failOn = "generate-access-token"

	job := events.NewJob()
	report, err := Drill(context.Background(), job, f.opts)
	if err == nil {
		t.Fatal("Drill succeeded with a failing smoke job, want error")
	}
	if report.Succeeded() {
		t.Fatal("report.Succeeded() = true with a failing smoke job")
	}
	if report.Failure.Step != forge.StepSmokeCI {
		t.Errorf("report.Failure.Step = %q, want %q", report.Failure.Step, forge.StepSmokeCI)
	}
	if !strings.Contains(err.Error(), forge.StepSmokeCI) {
		t.Errorf("error %q does not name the failing step", err)
	}

	// The restore itself succeeded, so nothing before the smoke job may be
	// reported as the failure.
	last := job.Events()[len(job.Events())-1]
	if last.State != events.StateFailed || !strings.Contains(last.Detail, forge.StepSmokeCI) {
		t.Errorf("terminal event = %+v, want a failure naming %s", last, forge.StepSmokeCI)
	}
}

// A drill that could not restore has nothing to run CI against: the smoke
// job must not be attempted, and the failure the operator sees stays the
// one that actually happened.
func TestDrillSkipsTheSmokeJobWhenTheRestoreFails(t *testing.T) {
	f := newFixture(t)
	f.host().failOn = "docker compose up -d"

	report, err := Drill(context.Background(), events.NewJob(), f.opts)
	if err == nil {
		t.Fatal("Drill succeeded with a failing converge, want error")
	}
	if report.Failure.Step == forge.StepSmokeCI {
		t.Errorf("report blames the smoke job for a converge failure: %+v", report.Failure)
	}
	if commands := smokeCommands(f.host()); len(commands) != 0 {
		t.Errorf("drill ran the smoke job against a stack that never booted: %v", commands)
	}
}
