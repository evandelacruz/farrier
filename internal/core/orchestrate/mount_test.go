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
