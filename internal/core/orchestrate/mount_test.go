package orchestrate

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWithBindMountAddsVolumeToNamedService(t *testing.T) {
	files, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	out, err := WithBindMount(files, "forgejo", "/opt/farrier/forge/app.ini", "/data/gitea/conf/app.ini")
	if err != nil {
		t.Fatalf("WithBindMount: %v", err)
	}

	var spec composeSpec
	if err := yaml.Unmarshal(out[ComposeFile], &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	forgejo, ok := spec.Services["forgejo"]
	if !ok {
		t.Fatalf("missing forgejo service: %+v", spec.Services)
	}
	if len(forgejo.Volumes) != 1 || forgejo.Volumes[0] != "/opt/farrier/forge/app.ini:/data/gitea/conf/app.ini" {
		t.Errorf("forgejo volumes = %v", forgejo.Volumes)
	}

	// caddy is untouched.
	caddy, ok := spec.Services["caddy"]
	if !ok {
		t.Fatalf("missing caddy service: %+v", spec.Services)
	}
	if len(caddy.Volumes) != 0 {
		t.Errorf("caddy volumes = %v, want none", caddy.Volumes)
	}
}

func TestWithBindMountDoesNotMutateInput(t *testing.T) {
	files, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	before := string(files[ComposeFile])

	if _, err := WithBindMount(files, "forgejo", "/host/app.ini", "/data/gitea/conf/app.ini"); err != nil {
		t.Fatalf("WithBindMount: %v", err)
	}

	if string(files[ComposeFile]) != before {
		t.Errorf("input file mutated by WithBindMount")
	}
}

func TestWithBindMountRejectsUnknownService(t *testing.T) {
	files, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, err := WithBindMount(files, "nonexistent", "/host/path", "/container/path"); err == nil {
		t.Fatal("WithBindMount: want error for unknown service, got nil")
	}
}

func TestWithEnvSetsVariableOnNamedService(t *testing.T) {
	files, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	out, err := WithEnv(files, "forgejo", "FARRIER_APP_INI_CHECKSUM", "deadbeef")
	if err != nil {
		t.Fatalf("WithEnv: %v", err)
	}

	var spec composeSpec
	if err := yaml.Unmarshal(out[ComposeFile], &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	forgejo, ok := spec.Services["forgejo"]
	if !ok {
		t.Fatalf("missing forgejo service: %+v", spec.Services)
	}
	if forgejo.Environment["FARRIER_APP_INI_CHECKSUM"] != "deadbeef" {
		t.Errorf("forgejo environment = %v, want FARRIER_APP_INI_CHECKSUM=deadbeef", forgejo.Environment)
	}
	// Render's own FARRIER_DOMAIN entry must survive the merge.
	if forgejo.Environment["FARRIER_DOMAIN"] == "" {
		t.Errorf("forgejo environment = %v, lost FARRIER_DOMAIN", forgejo.Environment)
	}

	caddy, ok := spec.Services["caddy"]
	if !ok {
		t.Fatalf("missing caddy service: %+v", spec.Services)
	}
	if _, set := caddy.Environment["FARRIER_APP_INI_CHECKSUM"]; set {
		t.Errorf("caddy environment = %v, want FARRIER_APP_INI_CHECKSUM unset", caddy.Environment)
	}
}

func TestWithEnvChangesOutputWhenValueChanges(t *testing.T) {
	files, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	a, err := WithEnv(files, "forgejo", "FARRIER_APP_INI_CHECKSUM", "aaaa")
	if err != nil {
		t.Fatalf("WithEnv: %v", err)
	}
	b, err := WithEnv(files, "forgejo", "FARRIER_APP_INI_CHECKSUM", "bbbb")
	if err != nil {
		t.Fatalf("WithEnv: %v", err)
	}
	if string(a[ComposeFile]) == string(b[ComposeFile]) {
		t.Error("WithEnv produced identical output for different values — a content-only app.ini change would never force a recreate")
	}
}

func TestWithEnvDoesNotMutateInput(t *testing.T) {
	files, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	before := string(files[ComposeFile])

	if _, err := WithEnv(files, "forgejo", "FARRIER_APP_INI_CHECKSUM", "deadbeef"); err != nil {
		t.Fatalf("WithEnv: %v", err)
	}

	if string(files[ComposeFile]) != before {
		t.Errorf("input file mutated by WithEnv")
	}
}

func TestWithEnvRejectsUnknownService(t *testing.T) {
	files, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, err := WithEnv(files, "nonexistent", "KEY", "value"); err == nil {
		t.Fatal("WithEnv: want error for unknown service, got nil")
	}
}

func TestWithPortsAddsPortToNamedService(t *testing.T) {
	files, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	out, err := WithPorts(files, "caddy", "443", "443")
	if err != nil {
		t.Fatalf("WithPorts: %v", err)
	}

	var spec composeSpec
	if err := yaml.Unmarshal(out[ComposeFile], &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	caddy, ok := spec.Services["caddy"]
	if !ok {
		t.Fatalf("missing caddy service: %+v", spec.Services)
	}
	if len(caddy.Ports) != 1 || caddy.Ports[0] != "443:443" {
		t.Errorf("caddy ports = %v", caddy.Ports)
	}

	// forgejo is untouched.
	forgejo, ok := spec.Services["forgejo"]
	if !ok {
		t.Fatalf("missing forgejo service: %+v", spec.Services)
	}
	if len(forgejo.Ports) != 0 {
		t.Errorf("forgejo ports = %v, want none", forgejo.Ports)
	}
}

func TestWithPortsDoesNotMutateInput(t *testing.T) {
	files, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	before := string(files[ComposeFile])

	if _, err := WithPorts(files, "caddy", "443", "443"); err != nil {
		t.Fatalf("WithPorts: %v", err)
	}

	if string(files[ComposeFile]) != before {
		t.Errorf("input file mutated by WithPorts")
	}
}

func TestWithPortsRejectsUnknownService(t *testing.T) {
	files, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, err := WithPorts(files, "nonexistent", "443", "443"); err == nil {
		t.Fatal("WithPorts: want error for unknown service, got nil")
	}
}

// TestWithLoopbackPortsBindsToLoopbackOnly is the Compose half of DRIL-002's
// "reachable only through an SSH tunnel": the published entry must carry an
// explicit bind address, because a bare host:container mapping binds every
// interface on the host.
func TestWithLoopbackPortsBindsToLoopbackOnly(t *testing.T) {
	files, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}

	out, err := WithLoopbackPorts(files, "caddy", "443", "443")
	if err != nil {
		t.Fatalf("WithLoopbackPorts: %v", err)
	}

	var spec composeSpec
	if err := yaml.Unmarshal(out[ComposeFile], &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	caddy, ok := spec.Services["caddy"]
	if !ok {
		t.Fatalf("missing caddy service: %+v", spec.Services)
	}
	want := LoopbackAddress + ":443:443"
	if len(caddy.Ports) != 1 || caddy.Ports[0] != want {
		t.Errorf("caddy ports = %v, want [%s]", caddy.Ports, want)
	}
}

func TestWithLoopbackPortsDoesNotMutateInput(t *testing.T) {
	files, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	before := string(files[ComposeFile])

	if _, err := WithLoopbackPorts(files, "caddy", "443", "443"); err != nil {
		t.Fatalf("WithLoopbackPorts: %v", err)
	}

	if string(files[ComposeFile]) != before {
		t.Errorf("input file mutated by WithLoopbackPorts")
	}
}

func TestWithLoopbackPortsRejectsUnknownService(t *testing.T) {
	files, err := Render(testManifest())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if _, err := WithLoopbackPorts(files, "nonexistent", "443", "443"); err == nil {
		t.Fatal("WithLoopbackPorts: want error for unknown service, got nil")
	}
}
