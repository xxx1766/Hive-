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
	Image   string         `yaml:"image"`          // name:version
	Rank    string         `yaml:"rank,omitempty"` // overrides manifest default
	Quota   map[string]any `yaml:"quota,omitempty"`
	Volumes []VolumeMount  `yaml:"volumes,omitempty"`
}

// VolumeMount binds a named Volume into the Agent's sandbox.
//
//   name:        existing Volume (created via `hive volume create`)
//   mode:        "ro" (read-only) or "rw" (writable)
//   mountpoint:  absolute path inside the sandbox where the Volume appears
type VolumeMount struct {
	Name       string `yaml:"name"`
	Mode       string `yaml:"mode"`
	Mountpoint string `yaml:"mountpoint"`
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
		// Remote refs resolve at `hive hire -f` time (name comes from the
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
	// Validate each agent's volume mounts. Iterate by index so Validate's
	// mutation (default mode=ro) actually persists back into the slice.
	for i := range f.Agents {
		for j := range f.Agents[i].Volumes {
			if err := f.Agents[i].Volumes[j].Validate(); err != nil {
				return fmt.Errorf("agents[%d].volumes[%d]: %w", i, j, err)
			}
		}
	}
	return nil
}

// Validate checks a VolumeMount's shape independent of whether the
// named Volume exists (that's a daemon-side check at hire time).
func (v *VolumeMount) Validate() error {
	if v.Name == "" {
		return fmt.Errorf("volume.name is required")
	}
	if v.Mountpoint == "" {
		return fmt.Errorf("volume.mountpoint is required")
	}
	if v.Mountpoint[0] != '/' {
		return fmt.Errorf("volume.mountpoint must be absolute, got %q", v.Mountpoint)
	}
	if containsDotDot(v.Mountpoint) {
		return fmt.Errorf("volume.mountpoint must not contain '..': %q", v.Mountpoint)
	}
	switch v.Mode {
	case "ro", "rw":
	case "":
		v.Mode = "ro" // safe default
	default:
		return fmt.Errorf("volume.mode must be ro|rw, got %q", v.Mode)
	}
	return nil
}

func containsDotDot(p string) bool {
	// Simple guard; the path is already required to be absolute.
	for i := 0; i+1 < len(p); i++ {
		if p[i] == '.' && p[i+1] == '.' {
			return true
		}
	}
	return false
}

func lastSegment(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[i+1:]
		}
	}
	return path
}
