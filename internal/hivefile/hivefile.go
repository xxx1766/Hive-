// Package hivefile parses Hivefile.yaml — the declarative manifest that
// says which Agents a Room hires and how they should be ranked/quota-capped.
//
// Hivefile is to Hive what docker-compose.yaml is to Docker.
package hivefile

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/anne-x/hive/internal/image"
	"github.com/anne-x/hive/internal/remote"
)

// File is the in-memory form of a Hivefile.yaml.
type File struct {
	Room   string       `yaml:"room"`
	Agents []AgentEntry `yaml:"agents"`
	Entry  string       `yaml:"entry,omitempty"` // image name of the Agent `hive run` defaults to
}

// AgentEntry declares one Agent to hire.
type AgentEntry struct {
	Image string         `yaml:"image"`          // name:version
	Rank  string         `yaml:"rank,omitempty"` // overrides manifest default
	Quota map[string]any `yaml:"quota,omitempty"`
}

// Load reads and validates a Hivefile.yaml.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := f.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

// Validate catches common mistakes at parse time so they don't surface
// as opaque IPC errors later.
func (f *File) Validate() error {
	if f.Room == "" {
		return fmt.Errorf("hivefile.room is required")
	}
	if len(f.Agents) == 0 {
		return fmt.Errorf("hivefile.agents is empty")
	}
	seen := make(map[string]bool)
	for i, a := range f.Agents {
		if a.Image == "" {
			return fmt.Errorf("agents[%d].image is required", i)
		}
		// Remote refs resolve at `hive up` time (name comes from the
		// fetched agent.yaml). Use the tail segment as a provisional name
		// for duplicate detection.
		if remote.LooksRemote(a.Image) {
			rref, err := remote.ParseRef(a.Image)
			if err != nil {
				return fmt.Errorf("agents[%d]: %w", i, err)
			}
			name := lastSegment(rref.Path)
			if seen[name] {
				return fmt.Errorf("agents[%d]: duplicate remote ref %q (tail %q collides)", i, a.Image, name)
			}
			seen[name] = true
			continue
		}
		ref, err := image.ParseRef(a.Image)
		if err != nil {
			return fmt.Errorf("agents[%d]: %w", i, err)
		}
		if seen[ref.Name] {
			return fmt.Errorf("agents[%d]: duplicate image name %q (only one of each per Room)", i, ref.Name)
		}
		seen[ref.Name] = true
	}
	if f.Entry != "" && !seen[f.Entry] {
		return fmt.Errorf("entry %q is not one of the hired agents", f.Entry)
	}
	return nil
}

func lastSegment(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}
