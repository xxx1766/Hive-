// Package store is a local image registry on disk.
//
// Layout:
//
//	<root>/<name>/<version>/
//	    agent.yaml
//	    bin/<entry>
//	    ...
//
// Demo only: no garbage collection, no integrity checks, no push/pull.
package store

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/anne-x/hive/internal/image"
)

type Store struct {
	Root string
}

func New(root string) *Store { return &Store{Root: root} }

// Put copies the source directory into the store keyed by its manifest's
// name:version. It overwrites any existing version.
func (s *Store) Put(sourceDir string) (*image.Image, error) {
	manifest, err := image.LoadManifest(sourceDir)
	if err != nil {
		return nil, err
	}
	dst := s.pathOf(image.Ref{Name: manifest.Name, Version: manifest.Version})
	if err := os.RemoveAll(dst); err != nil {
		return nil, fmt.Errorf("clean dst: %w", err)
	}
	if err := copyTree(sourceDir, dst); err != nil {
		return nil, fmt.Errorf("copy tree: %w", err)
	}
	return image.Load(dst)
}

// Get loads an Image by ref. Returns an error with ErrCodeImageNotFound-style
// wording if missing (the IPC layer maps this to the right error code).
func (s *Store) Get(ref image.Ref) (*image.Image, error) {
	dir := s.pathOf(ref)
	if _, err := os.Stat(dir); err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("image not found: %s", ref)
		}
		return nil, err
	}
	return image.Load(dir)
}

// List returns every image currently in the store, in no particular order.
func (s *Store) List() ([]image.Ref, error) {
	entries, err := os.ReadDir(s.Root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var refs []image.Ref
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		versionDir := filepath.Join(s.Root, name)
		vs, err := os.ReadDir(versionDir)
		if err != nil {
			continue
		}
		for _, v := range vs {
			if v.IsDir() {
				refs = append(refs, image.Ref{Name: name, Version: v.Name()})
			}
		}
	}
	return refs, nil
}

func (s *Store) pathOf(ref image.Ref) string {
	return filepath.Join(s.Root, ref.Name, ref.Version)
}

// copyTree recursively copies src → dst, preserving file modes.
// For small Images (demo) this is fine. A future version can harden:
// symlink handling, device files, xattrs, etc.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target, d)
	})
}

func copyFile(src, dst string, d fs.DirEntry) error {
	info, err := d.Info()
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
