// Package image models a Hive Image: a packaged, runnable Agent.
//
// An Image on disk is a directory that contains:
//   - hive.yaml              (Manifest)
//   - any files referenced by Entry (typically a bin/<name> binary)
//
// This keeps the format simple for demo — no tarballs, no layers, no digests.
// A future version can layer an OCI-style distribution format on top without
// changing this struct.
package image

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// ManifestFilename is the required filename at the root of an Image.
const ManifestFilename = "hive.yaml"

// Manifest mirrors the on-disk hive.yaml schema.
type Manifest struct {
	Name         string       `yaml:"name"`
	Version      string       `yaml:"version"`
	Entry        string       `yaml:"entry"` // relative path inside the Image
	Rank         string       `yaml:"rank"`  // default Rank (may be overridden at hire time)
	Capabilities Capabilities `yaml:"capabilities,omitempty"`
	Quota        Quota        `yaml:"quota,omitempty"`
}

type Capabilities struct {
	Requires []string `yaml:"requires,omitempty"`
	Provides []string `yaml:"provides,omitempty"`
}

// Quota is the manifest-declared default quota. Keys are provider:model
// for tokens and endpoint label for api_calls. Values are absolute caps.
type Quota struct {
	Tokens   map[string]int `yaml:"tokens,omitempty"`
	APICalls map[string]int `yaml:"api_calls,omitempty"`
}

// Image is a Manifest plus the directory where its bytes live.
type Image struct {
	Manifest Manifest
	Dir      string
}

// Ref identifies an image by name:version.
type Ref struct {
	Name    string
	Version string
}

func (r Ref) String() string { return r.Name + ":" + r.Version }

// ParseRef splits "name:version". Returns an error if either part is missing.
func ParseRef(s string) (Ref, error) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return Ref{}, fmt.Errorf("image ref must be name:version, got %q", s)
	}
	return Ref{Name: s[:i], Version: s[i+1:]}, nil
}

// LoadManifest reads and validates hive.yaml from dir.
func LoadManifest(dir string) (Manifest, error) {
	path := filepath.Join(dir, ManifestFilename)
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read %s: %w", path, err)
	}
	var m Manifest
	if err := yaml.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// Validate checks required fields and sane defaults.
func (m *Manifest) Validate() error {
	if m.Name == "" {
		return fmt.Errorf("manifest.name is required")
	}
	if m.Version == "" {
		return fmt.Errorf("manifest.version is required")
	}
	if m.Entry == "" {
		return fmt.Errorf("manifest.entry is required")
	}
	if strings.Contains(m.Entry, "..") {
		return fmt.Errorf("manifest.entry must not contain '..': %q", m.Entry)
	}
	if m.Rank == "" {
		m.Rank = "intern" // safe default: lowest privilege
	}
	return nil
}

// Load reads an already-installed Image from a store directory.
func Load(dir string) (*Image, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	return &Image{Manifest: m, Dir: dir}, nil
}

// Ref returns the Ref form of this Image.
func (i *Image) Ref() Ref { return Ref{Name: i.Manifest.Name, Version: i.Manifest.Version} }

// EntryPath is the absolute path of the entry binary.
func (i *Image) EntryPath() string { return filepath.Join(i.Dir, i.Manifest.Entry) }
