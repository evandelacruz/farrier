package orchestrate

import (
	"fmt"

	"github.com/evandelacruz/farrier/internal/core/bundle"
	"gopkg.in/yaml.v3"
)

// ComposeFile is the name Render writes its output under, matching
// bundle.ComposeDir's expectation of one or more files under compose/.
const ComposeFile = "docker-compose.yml"

// networkName is the single Docker network every rendered service joins, so
// components can reach each other by service name.
const networkName = "farrier"

type composeSpec struct {
	Services map[string]composeService `yaml:"services"`
	Networks map[string]composeNetwork `yaml:"networks"`
}

type composeService struct {
	Image         string            `yaml:"image"`
	ContainerName string            `yaml:"container_name"`
	Restart       string            `yaml:"restart"`
	Networks      []string          `yaml:"networks"`
	Environment   map[string]string `yaml:"environment,omitempty"`
	// Volumes and Ports are never set by Render itself — see its doc
	// comment — but are declared here so WithBindMount's and WithPorts'
	// output round-trips through the same composeSpec shape the package's
	// own tests decode with.
	Volumes []string `yaml:"volumes,omitempty"`
	Ports   []string `yaml:"ports,omitempty"`
}

type composeNetwork struct {
	Name string `yaml:"name"`
}

// Render turns a manifest into a rendered Compose definition (ORCH-002):
// one service per declared, digest-pinned image, all on a shared network
// and carrying the bundle's domain as an environment variable. The result
// is keyed by filename, ready to assign directly to bundle.Bundle.Compose.
//
// Per-component specifics beyond the image itself — ports, volumes, config
// mounts — belong to the component's own configuration story (e.g.
// FORGE-001 for Forgejo's app.ini) and are layered on separately; Render
// only owns what the manifest itself declares.
func Render(m *bundle.Manifest) (map[string][]byte, error) {
	if m == nil {
		return nil, fmt.Errorf("orchestrate: render: manifest is required")
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("orchestrate: render: %w", err)
	}

	services := make(map[string]composeService, len(m.Images))
	for component, image := range m.Images {
		services[component] = composeService{
			Image:         image,
			ContainerName: "farrier-" + component,
			Restart:       "unless-stopped",
			Networks:      []string{networkName},
			Environment:   map[string]string{"FARRIER_DOMAIN": m.Domain},
		}
	}

	spec := composeSpec{
		Services: services,
		Networks: map[string]composeNetwork{
			networkName: {Name: networkName},
		},
	}

	out, err := yaml.Marshal(&spec)
	if err != nil {
		return nil, fmt.Errorf("orchestrate: render: encode compose: %w", err)
	}
	return map[string][]byte{ComposeFile: out}, nil
}
