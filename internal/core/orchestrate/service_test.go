package orchestrate

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// twoServiceCompose is a rendered definition with two services, so every
// test here can assert the untouched one is carried through unchanged.
func twoServiceCompose() map[string][]byte {
	return map[string][]byte{
		ComposeFile: []byte("services:\n  forgejo:\n    image: forge\n  runner:\n    image: run\n"),
	}
}

func decodeSpec(t *testing.T, files map[string][]byte) composeSpec {
	t.Helper()
	var spec composeSpec
	if err := yaml.Unmarshal(files[ComposeFile], &spec); err != nil {
		t.Fatalf("decode compose: %v", err)
	}
	return spec
}

func TestWithCommandSetsTheServicesCommand(t *testing.T) {
	files := twoServiceCompose()

	out, err := WithCommand(files, "runner", []string{"sh", "-ec", "exec forgejo-runner daemon"})
	if err != nil {
		t.Fatalf("WithCommand: %v", err)
	}

	spec := decodeSpec(t, out)
	got := spec.Services["runner"].Command
	want := []string{"sh", "-ec", "exec forgejo-runner daemon"}
	if len(got) != len(want) {
		t.Fatalf("command = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("command = %v, want %v", got, want)
		}
	}
	if spec.Services["forgejo"].Image != "forge" {
		t.Errorf("the other service changed: %+v", spec.Services["forgejo"])
	}
	if len(spec.Services["forgejo"].Command) != 0 {
		t.Errorf("the other service gained a command: %v", spec.Services["forgejo"].Command)
	}
}

func TestWithCommandLeavesTheInputUnchanged(t *testing.T) {
	files := twoServiceCompose()
	before := string(files[ComposeFile])

	if _, err := WithCommand(files, "runner", []string{"true"}); err != nil {
		t.Fatalf("WithCommand: %v", err)
	}
	if string(files[ComposeFile]) != before {
		t.Error("WithCommand mutated its input")
	}
}

func TestWithCommandRequiresAKnownServiceAndACommand(t *testing.T) {
	if _, err := WithCommand(twoServiceCompose(), "absent", []string{"true"}); err == nil {
		t.Error("WithCommand on an undeclared service succeeded, want error")
	}
	if _, err := WithCommand(twoServiceCompose(), "runner", nil); err == nil {
		t.Error("WithCommand with no command succeeded, want error")
	}
}

func TestWithUserSetsTheServicesUser(t *testing.T) {
	out, err := WithUser(twoServiceCompose(), "runner", "0:0")
	if err != nil {
		t.Fatalf("WithUser: %v", err)
	}

	spec := decodeSpec(t, out)
	if spec.Services["runner"].User != "0:0" {
		t.Errorf("user = %q, want %q", spec.Services["runner"].User, "0:0")
	}
	if spec.Services["forgejo"].User != "" {
		t.Errorf("the other service gained a user: %q", spec.Services["forgejo"].User)
	}
}

func TestWithUserRequiresAKnownServiceAndAUser(t *testing.T) {
	if _, err := WithUser(twoServiceCompose(), "absent", "0:0"); err == nil {
		t.Error("WithUser on an undeclared service succeeded, want error")
	}
	if _, err := WithUser(twoServiceCompose(), "runner", ""); err == nil {
		t.Error("WithUser with no user succeeded, want error")
	}
}

func TestWithoutServiceRemovesOnlyThatService(t *testing.T) {
	out, err := WithoutService(twoServiceCompose(), "runner")
	if err != nil {
		t.Fatalf("WithoutService: %v", err)
	}

	spec := decodeSpec(t, out)
	if _, ok := spec.Services["runner"]; ok {
		t.Error("runner is still declared")
	}
	if spec.Services["forgejo"].Image != "forge" {
		t.Errorf("forgejo changed or went missing: %+v", spec.Services["forgejo"])
	}
	if strings.Contains(string(out[ComposeFile]), "runner") {
		t.Errorf("rendered output still mentions the removed service:\n%s", out[ComposeFile])
	}
}

// The caller's intent is that the service not be deployed. A definition
// that never declared it already satisfies that, so it is not an error —
// deploy.configureRunner relies on this when a bundle pins no runner image.
func TestWithoutServiceIsANoOpWhenTheServiceIsAbsent(t *testing.T) {
	files := twoServiceCompose()

	out, err := WithoutService(files, "absent")
	if err != nil {
		t.Fatalf("WithoutService on an undeclared service: %v", err)
	}
	if string(out[ComposeFile]) != string(files[ComposeFile]) {
		t.Errorf("output changed:\n%s", out[ComposeFile])
	}
}

func TestWithoutServiceLeavesTheInputUnchanged(t *testing.T) {
	files := twoServiceCompose()
	before := string(files[ComposeFile])

	if _, err := WithoutService(files, "runner"); err != nil {
		t.Fatalf("WithoutService: %v", err)
	}
	if string(files[ComposeFile]) != before {
		t.Error("WithoutService mutated its input")
	}
}
