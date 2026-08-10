package forge

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/evandelacruz/farrier/internal/core/events"
)

func TestSmokeCICreatesARepositoryCommitsAWorkflowAndWaitsForTheRun(t *testing.T) {
	runner := &fakeRunner{}
	job := events.NewJob()

	result, err := SmokeCI(context.Background(), runner, job, SmokeOptions{Repository: "smoke-repo"})
	if err != nil {
		t.Fatalf("SmokeCI: %v", err)
	}
	if result.Repository != "admin/smoke-repo" {
		t.Errorf("Repository = %q, want %q", result.Repository, "admin/smoke-repo")
	}

	cmd := runner.gotCommand
	for _, want := range []string{
		// Driven from inside the forgejo container, over the same
		// `docker compose exec` path the rest of the package uses.
		"docker compose exec -T -u git forgejo sh -ec ",
		"http://localhost:3000/api/v1",
		// Create the scratch repository, on a branch the poll below can name.
		"send POST /user/repos ",
		`"name":"smoke-repo"`,
		`"auto_init":true`,
		`"default_branch":"main"`,
		// Actions on, whatever the snapshot's default repository units are.
		`{"has_actions":true}`,
		// Commit the workflow — the push that dispatches the run.
		"/repos/$owner/$repo/contents/" + smokeWorkflowPath,
		// Wait for the outcome.
		"/repos/$owner/$repo/commits/main/status",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command missing %q:\n%s", want, cmd)
		}
	}
}

// The workflow has to be the smallest thing that still proves a runner
// claimed the job and ran a step: no checkout, no actions to fetch, and a
// label the colocated runner answers to.
func TestSmokeCICommitsATrivialWorkflow(t *testing.T) {
	runner := &fakeRunner{}

	if _, err := SmokeCI(context.Background(), runner, events.NewJob(), SmokeOptions{}); err != nil {
		t.Fatalf("SmokeCI: %v", err)
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(smokeWorkflow()))
	if !strings.Contains(runner.gotCommand, encoded) {
		t.Fatalf("command does not carry the base64-encoded workflow:\n%s", runner.gotCommand)
	}

	workflow := smokeWorkflow()
	for _, want := range []string{"on: [push]", "runs-on: " + smokeRunsOn, "- run: echo"} {
		if !strings.Contains(workflow, want) {
			t.Errorf("workflow missing %q:\n%s", want, workflow)
		}
	}
	if strings.Contains(workflow, "uses:") {
		t.Errorf("workflow fetches an action, which a smoke job must not depend on:\n%s", workflow)
	}
}

// The token the script mints stays inside the container: Farrier builds a
// command with no credential in it, so nothing it can log — including
// transport error text that quotes the whole command — can carry one.
func TestSmokeCIMintsItsTokenInsideTheContainer(t *testing.T) {
	runner := &fakeRunner{}

	if _, err := SmokeCI(context.Background(), runner, events.NewJob(), SmokeOptions{}); err != nil {
		t.Fatalf("SmokeCI: %v", err)
	}

	if !strings.Contains(runner.gotCommand, "token=$(forgejo admin user generate-access-token") {
		t.Errorf("command does not mint its own token in-container:\n%s", runner.gotCommand)
	}
	if !strings.Contains(runner.gotCommand, `auth="Authorization: token $token"`) {
		t.Errorf("command does not use the minted token by reference:\n%s", runner.gotCommand)
	}
}

// Two drills against the same scratch target must not collide: DRIL-003's
// teardown is a separate requirement, so the second drill can find the first
// one's repository still there.
func TestSmokeCIGeneratesAFreshRepositoryNameEachCall(t *testing.T) {
	first, second := &fakeRunner{}, &fakeRunner{}

	a, err := SmokeCI(context.Background(), first, events.NewJob(), SmokeOptions{})
	if err != nil {
		t.Fatalf("SmokeCI: %v", err)
	}
	b, err := SmokeCI(context.Background(), second, events.NewJob(), SmokeOptions{})
	if err != nil {
		t.Fatalf("SmokeCI: %v", err)
	}
	if a.Repository == b.Repository {
		t.Fatalf("two calls used the same scratch repository %q", a.Repository)
	}
	if !strings.HasPrefix(a.Repository, "admin/farrier-drill-smoke-") {
		t.Errorf("Repository = %q, want an admin-owned farrier-drill-smoke- name", a.Repository)
	}
}

func TestSmokeCIEmitsAStepAndLeavesTheJobOpen(t *testing.T) {
	runner := &fakeRunner{}
	job := events.NewJob()

	if _, err := SmokeCI(context.Background(), runner, job, SmokeOptions{}); err != nil {
		t.Fatalf("SmokeCI: %v", err)
	}

	got := job.Events()
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (started, succeeded): %+v", len(got), got)
	}
	if got[0].Step != StepSmokeCI || got[0].State != events.StateStarted {
		t.Errorf("event 0 = %+v, want step=%s state=started", got[0], StepSmokeCI)
	}
	if got[1].Step != StepSmokeCI || got[1].State != events.StateSucceeded {
		t.Errorf("event 1 = %+v, want step=%s state=succeeded", got[1], StepSmokeCI)
	}
	if job.Done() {
		t.Error("SmokeCI ended the job; it should leave the terminal event to the caller")
	}
}

// DRIL-001's "the specific failing step" reaches down inside the smoke job
// too: what the script says broke is what the event and the error carry.
func TestSmokeCIFailureCarriesWhatTheScriptReported(t *testing.T) {
	cases := map[string]string{
		"repository": "create the scratch repository: the instance answered HTTP 409",
		"run failed": "the smoke run finished with state failure",
		"no runner":  "the smoke run did not finish within 10m0s (last state: none)",
	}
	for name, reported := range cases {
		t.Run(name, func(t *testing.T) {
			runner := &fakeRunner{stderr: reported, err: errors.New("exit status 1")}
			job := events.NewJob()

			_, err := SmokeCI(context.Background(), runner, job, SmokeOptions{})
			if err == nil {
				t.Fatal("SmokeCI succeeded, want error")
			}
			if !strings.Contains(err.Error(), reported) {
				t.Errorf("error %q does not carry what the script reported", err)
			}

			last := job.Events()[len(job.Events())-1]
			if last.State != events.StateFailed || last.Step != StepSmokeCI {
				t.Fatalf("last event = %+v, want step=%s state=failed", last, StepSmokeCI)
			}
			if !strings.Contains(last.Detail, reported) {
				t.Errorf("event detail %q does not carry what the script reported", last.Detail)
			}
		})
	}
}

// curl and the forgejo CLI write their own noise to stderr before the
// script's own message; the last line is the one that names what broke.
func TestSmokeCIFailureReportsTheScriptsOwnLastMessage(t *testing.T) {
	runner := &fakeRunner{
		stderr: "curl: (22) The requested URL returned error: 409\ncreate the scratch repository: the instance answered HTTP 409\n",
		err:    errors.New("exit status 1"),
	}

	_, err := SmokeCI(context.Background(), runner, events.NewJob(), SmokeOptions{})
	if err == nil {
		t.Fatal("SmokeCI succeeded, want error")
	}
	if strings.Contains(err.Error(), "curl: (22)") {
		t.Errorf("error leads with transport noise rather than the script's own message: %v", err)
	}
}

// The script writes its own failures to stderr, but a CLI aborting under it
// can print to stdout instead — a drill that reported "no output" for that
// would say nothing about why the restored instance cannot run a job.
func TestSmokeCIFailureReportsAMessageArrivingOnStdout(t *testing.T) {
	runner := &fakeRunner{stdout: "Forgejo is not supposed to be run as root. Sorry.", err: errors.New("exit status 1")}

	_, err := SmokeCI(context.Background(), runner, events.NewJob(), SmokeOptions{})
	if err == nil {
		t.Fatal("SmokeCI succeeded, want error")
	}
	if !strings.Contains(err.Error(), "not supposed to be run as root") {
		t.Errorf("error %q does not carry what the command printed", err)
	}
}

func TestSmokeCIFailureWithNoOutputStillReportsSomething(t *testing.T) {
	runner := &fakeRunner{err: errors.New("ssh: connection lost")}

	_, err := SmokeCI(context.Background(), runner, events.NewJob(), SmokeOptions{})
	if err == nil {
		t.Fatal("SmokeCI succeeded, want error")
	}
	if !strings.Contains(err.Error(), "command failed with no output") {
		t.Errorf("error = %v, want the no-output fallback", err)
	}
}

func TestSmokeOptionsDefaultTheWait(t *testing.T) {
	var zero SmokeOptions
	if zero.timeout() != defaultSmokeTimeout {
		t.Errorf("timeout() = %s, want %s", zero.timeout(), defaultSmokeTimeout)
	}
	if zero.poll() != defaultSmokePoll {
		t.Errorf("poll() = %s, want %s", zero.poll(), defaultSmokePoll)
	}

	set := SmokeOptions{Timeout: 90 * time.Second, Poll: time.Second}
	if set.timeout() != 90*time.Second || set.poll() != time.Second {
		t.Errorf("timeout()/poll() = %s/%s, want 1m30s/1s", set.timeout(), set.poll())
	}
}

func TestSmokeScriptUsesTheConfiguredWait(t *testing.T) {
	runner := &fakeRunner{}

	if _, err := SmokeCI(context.Background(), runner, events.NewJob(), SmokeOptions{
		Timeout: 90 * time.Second,
		Poll:    3 * time.Second,
	}); err != nil {
		t.Fatalf("SmokeCI: %v", err)
	}

	for _, want := range []string{"+ 90))", "sleep 3"} {
		if !strings.Contains(runner.gotCommand, want) {
			t.Errorf("command missing %q:\n%s", want, runner.gotCommand)
		}
	}
}
