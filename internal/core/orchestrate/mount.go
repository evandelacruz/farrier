package orchestrate

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// WithBindMount returns a copy of files with an additional host-path bind
// mount added to service's Compose definition: hostPath on the deploy host,
// mounted at containerPath inside the container.
//
// Render only owns what the manifest itself declares — image, network,
// restart policy — and deliberately leaves ports, volumes, and config
// mounts to "the component's own configuration story" (Render's doc
// comment). WithBindMount is that layering step: it lets deploy-time
// content that must never become bundle content (KEY-003 — an app.ini
// rendered from resolved key material, FORGE-001) reach a container
// without Render ever needing to know about it.
//
// files is read, never mutated; the returned map is a new one carrying the
// change plus every other file unchanged. It is an error if no file in
// files declares the named service.
func WithBindMount(files map[string][]byte, service, hostPath, containerPath string) (map[string][]byte, error) {
	return withServiceListEntry(files, "with bind mount", service, "volumes", fmt.Sprintf("%s:%s", hostPath, containerPath))
}

// WithPorts returns a copy of files with an additional host:container port
// published on service's Compose definition — Caddy's HTTPS port, so the
// forge is reachable from outside the deploy host (UP-002).
//
// Same layering rationale as WithBindMount: Render leaves ports to "the
// component's own configuration story", and this is that story's port
// counterpart.
func WithPorts(files map[string][]byte, service, hostPort, containerPort string) (map[string][]byte, error) {
	return withServiceListEntry(files, "with ports", service, "ports", fmt.Sprintf("%s:%s", hostPort, containerPort))
}

// LoopbackAddress is the host interface WithLoopbackPorts binds a published
// port to. Exported so a caller reasoning about reachability — or a test
// asserting it — names the same address this package emits.
const LoopbackAddress = "127.0.0.1"

// WithLoopbackPorts is WithPorts bound to the deploy host's loopback
// interface: the port is published as 127.0.0.1:hostPort->containerPort
// rather than on every interface.
//
// A port published this way is reachable from processes on the host itself
// and from an SSH tunnel terminating there, and from nowhere else — the
// host's firewall, or the absence of one, does not enter into it, because
// nothing is ever bound to a routable address. That is what DRIL-002's
// "reachable only through an SSH tunnel" asks for, and why a drilled
// instance gets this instead of WithPorts (deploy.configureTLS).
func WithLoopbackPorts(files map[string][]byte, service, hostPort, containerPort string) (map[string][]byte, error) {
	return withServiceListEntry(files, "with loopback ports", service, "ports", fmt.Sprintf("%s:%s:%s", LoopbackAddress, hostPort, containerPort))
}

// withServiceListEntry appends entry to field (a Compose list key such as
// "volumes" or "ports") on service's definition across files, returning a
// new map with the change; files itself is never mutated. op names the
// caller in error messages. It is an error if no file in files declares the
// named service.
func withServiceListEntry(files map[string][]byte, op, service, field, entry string) (map[string][]byte, error) {
	for name, raw := range files {
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("orchestrate: %s: parse %s: %w", op, name, err)
		}

		services, _ := doc["services"].(map[string]any)
		svc, ok := services[service].(map[string]any)
		if !ok {
			continue
		}

		existing, _ := svc[field].([]any)
		svc[field] = append(existing, entry)
		services[service] = svc
		doc["services"] = services

		out, err := yaml.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("orchestrate: %s: encode %s: %w", op, name, err)
		}

		result := make(map[string][]byte, len(files))
		for k, v := range files {
			result[k] = v
		}
		result[name] = out
		return result, nil
	}
	return nil, fmt.Errorf("orchestrate: %s: no Compose file declares service %q", op, service)
}

// WithEnv returns a copy of files with key=value set in service's Compose
// environment, merged into whatever environment entries are already there.
//
// It exists for deploy-time content that is bind-mounted rather than
// declared in the Compose definition itself (WithBindMount's doc comment —
// app.ini, FORGE-001): `docker compose up -d` decides whether to recreate a
// service by diffing its resolved config — image, environment, volumes,
// labels — never the bytes of a file a volume happens to point at. So a
// content-only change to that file is invisible to Converge and the
// running container keeps serving the old config (UP-003: re-running `up`
// must actually converge the host, not just avoid erroring). Deploy calls
// WithEnv with a checksum of the file it just shipped so a content change
// changes the service's resolved config too, giving Converge something to
// diff.
//
// files is read, never mutated; the returned map is a new one carrying the
// change plus every other file unchanged. It is an error if no file in
// files declares the named service.
func WithEnv(files map[string][]byte, service, key, value string) (map[string][]byte, error) {
	for name, raw := range files {
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("orchestrate: with env: parse %s: %w", name, err)
		}

		services, _ := doc["services"].(map[string]any)
		svc, ok := services[service].(map[string]any)
		if !ok {
			continue
		}

		env, _ := svc["environment"].(map[string]any)
		if env == nil {
			env = map[string]any{}
		}
		env[key] = value
		svc["environment"] = env
		services[service] = svc
		doc["services"] = services

		out, err := yaml.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("orchestrate: with env: encode %s: %w", name, err)
		}

		result := make(map[string][]byte, len(files))
		for k, v := range files {
			result[k] = v
		}
		result[name] = out
		return result, nil
	}
	return nil, fmt.Errorf("orchestrate: with env: no Compose file declares service %q", service)
}
