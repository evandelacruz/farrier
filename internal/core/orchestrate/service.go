package orchestrate

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// WithCommand returns a copy of files with command set as service's Compose
// command, replacing whatever the image's own entrypoint would have run.
//
// Same layering rationale as WithBindMount: Render owns only what the
// manifest declares, and a component whose configuration story needs more
// than an image reference — the colocated Actions runner, which derives its
// credentials from a mounted secret before starting its daemon (FORGE-005,
// forge.RunnerCommand) — has it layered on at deploy time.
//
// command is written in list form, so Compose passes its elements through
// unchanged rather than word-splitting a string, and a change to it changes
// the service's resolved config — which is what makes Converge recreate the
// container (WithEnv's doc comment).
//
// files is read, never mutated; the returned map is a new one carrying the
// change plus every other file unchanged. It is an error if no file in
// files declares the named service.
func WithCommand(files map[string][]byte, service string, command []string) (map[string][]byte, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("orchestrate: with command: command is required")
	}
	entries := make([]any, len(command))
	for i, arg := range command {
		entries[i] = arg
	}
	return withServiceField(files, "with command", service, "command", entries)
}

// WithUser returns a copy of files with user set as the user service's
// container runs as, in Compose's "uid:gid" or name form.
//
// files is read, never mutated; the returned map is a new one carrying the
// change plus every other file unchanged. It is an error if no file in
// files declares the named service.
func WithUser(files map[string][]byte, service, user string) (map[string][]byte, error) {
	if user == "" {
		return nil, fmt.Errorf("orchestrate: with user: user is required")
	}
	return withServiceField(files, "with user", service, "user", user)
}

// WithoutService returns a copy of files with service removed from every
// Compose file that declares it.
//
// A bundle's Compose definition is rendered once, at init, from the
// manifest's image list; a later manifest change that means "do not deploy
// this component" — actions.colocatedRunner set to false (FORGE-005) — has
// no way to reach that rendered definition other than deploy time. Removing
// the service here, rather than never rendering it, is also what makes the
// change take effect on a host that already runs it: Converge's
// `docker compose up -d --remove-orphans` removes a container whose service
// is no longer declared.
//
// Removing a service no file declares is not an error: the caller's intent
// is that it not be deployed, and it isn't.
//
// files is read, never mutated; the returned map is a new one carrying the
// change plus every other file unchanged.
func WithoutService(files map[string][]byte, service string) (map[string][]byte, error) {
	result := make(map[string][]byte, len(files))
	for name, raw := range files {
		result[name] = raw
	}

	for name, raw := range files {
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("orchestrate: without service: parse %s: %w", name, err)
		}

		services, _ := doc["services"].(map[string]any)
		if _, ok := services[service]; !ok {
			continue
		}
		delete(services, service)
		doc["services"] = services

		out, err := yaml.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("orchestrate: without service: encode %s: %w", name, err)
		}
		result[name] = out
	}
	return result, nil
}

// withServiceField sets field to value on service's definition across
// files, returning a new map with the change; files itself is never
// mutated. op names the caller in error messages. It is an error if no file
// in files declares the named service.
//
// It is withServiceListEntry's set-a-whole-value counterpart: that one
// appends to a Compose list key that may already carry entries from an
// earlier layering step, this one replaces a single-valued key outright.
func withServiceField(files map[string][]byte, op, service, field string, value any) (map[string][]byte, error) {
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

		svc[field] = value
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
