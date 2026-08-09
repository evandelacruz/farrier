package orchestrate

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// defaultNetwork is Compose's implicit network — the one a service that
// declares no networks of its own is on.
const defaultNetwork = "default"

// WithNetworkAlias returns a copy of files with alias added as an extra name
// service answers to on every Docker network it joins: containers on those
// networks resolve alias to this service, exactly as they resolve its
// service name.
//
// It exists for the one case where a deployment must be self-contained
// rather than reachable the way the outside world reaches it: a drilled
// instance (DRIL-002). The colocated Actions runner connects to the bundle
// domain (forge.RunnerInstanceURL), and a drill deliberately leaves DNS
// pointing at production (DRIL-001) — so without an alias the runner beside
// a drilled instance would resolve that domain to production and poll
// production's job queue with the bundle's own runner secret. Aliasing the
// domain onto the drilled Caddy keeps the resolution local to the drilled
// host's Docker network, without touching a DNS record, publishing a port,
// or changing a single byte of the runner's own configuration.
//
// The alias is added to each network the service declares, since a service
// answers to a name per network. Render puts every service on one shared
// network, so in practice that is one entry; doing it per network rather
// than assuming that keeps the helper correct if a later manifest declares
// more.
//
// Compose accepts a service's networks either as a list of names or as a
// mapping from name to per-network options; aliases only exist in the
// mapping form, so a service still in list form is converted. files is read,
// never mutated; the returned map is a new one carrying the change plus
// every other file unchanged. It is an error if no file in files declares
// the named service.
func WithNetworkAlias(files map[string][]byte, service, alias string) (map[string][]byte, error) {
	if alias == "" {
		return nil, fmt.Errorf("orchestrate: with network alias: alias is required")
	}

	for name, raw := range files {
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			return nil, fmt.Errorf("orchestrate: with network alias: parse %s: %w", name, err)
		}

		services, _ := doc["services"].(map[string]any)
		svc, ok := services[service].(map[string]any)
		if !ok {
			continue
		}

		networks, err := networksWithAlias(svc["networks"], alias)
		if err != nil {
			return nil, fmt.Errorf("orchestrate: with network alias: service %q: %w", service, err)
		}
		svc["networks"] = networks
		services[service] = svc
		doc["services"] = services

		out, err := yaml.Marshal(doc)
		if err != nil {
			return nil, fmt.Errorf("orchestrate: with network alias: encode %s: %w", name, err)
		}

		result := make(map[string][]byte, len(files))
		for k, v := range files {
			result[k] = v
		}
		result[name] = out
		return result, nil
	}
	return nil, fmt.Errorf("orchestrate: with network alias: no Compose file declares service %q", service)
}

// networksWithAlias returns the service's networks key with alias appended
// to every network's alias list, converting the list form Render emits into
// the mapping form aliases require. An existing mapping keeps whatever
// options it already carries.
func networksWithAlias(existing any, alias string) (map[string]any, error) {
	out := map[string]any{}

	switch networks := existing.(type) {
	case []any:
		for _, entry := range networks {
			name, ok := entry.(string)
			if !ok {
				return nil, fmt.Errorf("network entry %v is not a name", entry)
			}
			out[name] = map[string]any{"aliases": []any{alias}}
		}
	case map[string]any:
		for name, opts := range networks {
			options, _ := opts.(map[string]any)
			if options == nil {
				options = map[string]any{}
			}
			aliases, _ := options["aliases"].([]any)
			options["aliases"] = append(aliases, alias)
			out[name] = options
		}
	case nil:
		// A service that declares no networks is on Compose's implicit
		// default network, so naming it explicitly is where the alias goes —
		// and saying so changes nothing about which network the service is
		// on.
		out[defaultNetwork] = map[string]any{"aliases": []any{alias}}
	default:
		return nil, fmt.Errorf("networks key is neither a list nor a mapping")
	}

	if len(out) == 0 {
		return nil, fmt.Errorf("joins no network to alias on")
	}
	return out, nil
}
