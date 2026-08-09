package forge

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The smoke job is a shell script running inside a container Farrier cannot
// see the inside of, so these tests run it for real against stand-ins for
// the two commands it calls — Forgejo's admin CLI and curl — and assert on
// what it does with their answers. A script that only ever ran on a drilled
// host would otherwise be the one part of DRIL-001 nothing tests.

// fakeInstance builds a directory holding a `forgejo` and a `curl` the smoke
// script will find ahead of any real ones, and returns it. curlBody is the
// shell body of the fake curl: it is handed the request's URL in $url and
// prints what the instance would answer.
func fakeInstance(t *testing.T, curlBody string) string {
	t.Helper()
	dir := t.TempDir()

	write := func(name, body string) {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatalf("write fake %s: %v", name, err)
		}
	}

	write("forgejo", `echo fake-token-value`)
	// The URL is curl's last argument in every call the script makes.
	write("curl", "url=\"\"\nfor a in \"$@\"; do url=\"$a\"; done\n"+curlBody)
	return dir
}

// runSmokeScript runs the script with dir at the front of PATH and returns
// its combined output and whether it succeeded.
func runSmokeScript(t *testing.T, script, dir string) (string, error) {
	t.Helper()
	cmd := exec.Command("sh", "-ec", script)
	cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// ok answers every write request 200 and every status poll with state.
func ok(state string) string {
	return `case "$url" in
  */status) printf '{"state":"` + state + `","sha":"abc","total_count":1,"statuses":[{"status":"` + state + `","state":"` + state + `"}]}' ;;
  *) printf '{}\n200' ;;
esac`
}

func TestSmokeScriptSucceedsWhenTheRunSucceeds(t *testing.T) {
	script := smokeScript("smoke-repo", "smoke-token", 30*time.Second, time.Second)

	out, err := runSmokeScript(t, script, fakeInstance(t, ok("success")))
	if err != nil {
		t.Fatalf("script failed on a successful run: %v\n%s", err, out)
	}
}

// The poll is a loop, not a single look: a run that is still pending when
// first asked about is the normal case, since the drilled host is usually
// still pulling the job container.
func TestSmokeScriptWaitsOutAPendingRun(t *testing.T) {
	dir := fakeInstance(t, `case "$url" in
  */status)
    if [ -f "$TMPDIR_MARKER" ]; then
      printf '{"state":"success"}'
    else
      touch "$TMPDIR_MARKER"
      printf '{"state":"pending","statuses":[]}'
    fi ;;
  *) printf '{}\n200' ;;
esac`)
	marker := filepath.Join(t.TempDir(), "polled")
	t.Setenv("TMPDIR_MARKER", marker)

	script := smokeScript("smoke-repo", "smoke-token", 30*time.Second, time.Second)
	out, err := runSmokeScript(t, script, dir)
	if err != nil {
		t.Fatalf("script failed while waiting out a pending run: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("script never polled the run's status: %v", statErr)
	}
}

func TestSmokeScriptReportsAFailedRun(t *testing.T) {
	script := smokeScript("smoke-repo", "smoke-token", 30*time.Second, time.Second)

	out, err := runSmokeScript(t, script, fakeInstance(t, ok("failure")))
	if err == nil {
		t.Fatalf("script succeeded on a failed run:\n%s", out)
	}
	if !strings.Contains(out, "the smoke run finished with state failure") {
		t.Errorf("output does not name the failed run:\n%s", out)
	}
}

// A run nothing ever claims is DRIL-001's other failure mode, and the report
// has to say so rather than blaming the restore.
func TestSmokeScriptReportsARunNothingClaims(t *testing.T) {
	script := smokeScript("smoke-repo", "smoke-token", time.Second, time.Second)

	out, err := runSmokeScript(t, script, fakeInstance(t, ok("")))
	if err == nil {
		t.Fatalf("script succeeded on a run that never started:\n%s", out)
	}
	for _, want := range []string{"did not finish within 1s", "last state: none", smokeRunsOn} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestSmokeScriptReportsARejectedRequest(t *testing.T) {
	script := smokeScript("smoke-repo", "smoke-token", 30*time.Second, time.Second)

	out, err := runSmokeScript(t, script, fakeInstance(t, `printf '{"message":"repository already exists"}\n409'`))
	if err == nil {
		t.Fatalf("script succeeded against an instance that rejected it:\n%s", out)
	}
	for _, want := range []string{"create the scratch repository", "HTTP 409", "repository already exists"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestSmokeScriptReportsAnUnreachableInstance(t *testing.T) {
	script := smokeScript("smoke-repo", "smoke-token", 30*time.Second, time.Second)

	out, err := runSmokeScript(t, script, fakeInstance(t, `exit 7`))
	if err == nil {
		t.Fatalf("script succeeded against an unreachable instance:\n%s", out)
	}
	if !strings.Contains(out, "could not reach the instance") {
		t.Errorf("output does not say the instance was unreachable:\n%s", out)
	}
}

// The token the CLI mints is used, not re-derived or assumed: a script that
// silently sent unauthenticated requests would pass every other test here
// and fail on a real instance.
func TestSmokeScriptSendsTheMintedToken(t *testing.T) {
	dir := fakeInstance(t, `for a in "$@"; do
  case "$a" in "Authorization: token fake-token-value") echo authenticated >> "$TMPDIR_MARKER" ;; esac
done
case "$url" in
  */status) printf '{"state":"success"}' ;;
  *) printf '{}\n200' ;;
esac`)
	marker := filepath.Join(t.TempDir(), "auth")
	t.Setenv("TMPDIR_MARKER", marker)

	script := smokeScript("smoke-repo", "smoke-token", 30*time.Second, time.Second)
	if out, err := runSmokeScript(t, script, dir); err != nil {
		t.Fatalf("script: %v\n%s", err, out)
	}

	seen, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("no request carried the minted token: %v", err)
	}
	if got := strings.Count(string(seen), "authenticated"); got != 4 {
		t.Errorf("%d of the script's 4 requests carried the minted token", got)
	}
}

// The script travels to the host as one argument of a shell command, so its
// quoting has to survive a real shell — a script mangled on the way would
// fail on a drilled host and nowhere else.
func TestSmokeCommandSurvivesTheRemoteShell(t *testing.T) {
	dir := t.TempDir()
	captured := filepath.Join(dir, "script")
	// A `docker` that writes the script it was handed, exactly as the shell
	// delivered it, and does nothing else.
	docker := "#!/bin/sh\nfor a in \"$@\"; do script=\"$a\"; done\nprintf '%s' \"$script\" > " + captured + "\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(docker), 0o755); err != nil {
		t.Fatalf("write fake docker: %v", err)
	}

	command := smokeCommand("smoke-repo", "smoke-token", 30*time.Second, time.Second)
	if out, err := runSmokeScript(t, command, dir); err != nil {
		t.Fatalf("running the smoke command: %v\n%s", err, out)
	}

	got, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("fake docker captured nothing: %v", err)
	}
	if want := smokeScript("smoke-repo", "smoke-token", 30*time.Second, time.Second); string(got) != want {
		t.Errorf("the shell delivered a different script:\n%s\nwant\n%s", got, want)
	}
}
