// Package volume owns Hive's named persistent storage. A Volume is a
// directory on disk (~/.hive/volumes/<name>/) that survives across Room
// lifetimes and can be targeted by the memory/* Agent API from any Room.
//
// MVP scope:
//   - CLI-managed lifecycle (`hive volume create/ls/rm`)
//   - KV-shaped access only (filesystem bind-mount of volumes into
//     sandboxes is deferred to a follow-up)
//   - No size / quota limits (trust users; add per-volume caps later)
//   - No ACL — any Room that knows the volume name can read/write it.
//     Production: declare in Hivefile to make access auditable.
package volume

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// nameRe enforces conservative volume names: alphanumerics + dash +
// underscore, 1-64 chars. Keeps filenames/paths predictable on all fs.
var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Volume is a named on-disk persistent container.
type Volume struct {
	Name      string    `json:"name"`
	Path      string    `json:"path"`
	CreatedAt time.Time `json:"created_at"`
}

// Manager holds all Volumes under a single root (daemon-wide).
type Manager struct {
	Root string // e.g. ~/.hive/volumes
}

// New returns a Manager rooted at `root`, ensuring the dir exists.
func New(root string) (*Manager, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, fmt.Errorf("volume: mkdir root: %w", err)
	}
	return &Manager{Root: root}, nil
}

// Create makes a new Volume. Fails if the name is invalid or already taken.
func (m *Manager) Create(name string) (*Volume, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("volume: invalid name %q (want [A-Za-z0-9_-]{1,64})", name)
	}
	path := filepath.Join(m.Root, name)
	if _, err := os.Stat(path); err == nil {
		return nil, fmt.Errorf("volume: %q already exists", name)
	}
	// memory/ subdir is where memproxy stores KV entries.
	if err := os.MkdirAll(filepath.Join(path, "memory"), 0o750); err != nil {
		return nil, fmt.Errorf("volume: mkdir: %w", err)
	}
	return &Volume{Name: name, Path: path, CreatedAt: time.Now()}, nil
}

// Get returns an existing Volume or an error if missing.
func (m *Manager) Get(name string) (*Volume, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("volume: invalid name %q", name)
	}
	path := filepath.Join(m.Root, name)
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("volume: %q not found", name)
		}
		return nil, err
	}
	return &Volume{Name: name, Path: path, CreatedAt: info.ModTime()}, nil
}

// List enumerates every Volume, sorted by name.
func (m *Manager) List() ([]Volume, error) {
	entries, err := os.ReadDir(m.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Volume
	for _, e := range entries {
		if !e.IsDir() || !nameRe.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Volume{
			Name:      e.Name(),
			Path:      filepath.Join(m.Root, e.Name()),
			CreatedAt: info.ModTime(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Remove deletes the Volume and everything in it. Returns nil if the
// volume didn't exist — matching rm -f semantics. Callers who need
// strict "exists" should Get first.
func (m *Manager) Remove(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("volume: invalid name %q", name)
	}
	return os.RemoveAll(filepath.Join(m.Root, name))
}

// MemoryDir is where memory/* proxy writes KV entries for this Volume.
// Kept as a helper so the proxy package doesn't need to know the layout.
func (v *Volume) MemoryDir() string { return filepath.Join(v.Path, "memory") }
