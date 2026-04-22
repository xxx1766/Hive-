package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/anne-x/hive/internal/image"
)

func TestPutGetList(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "agent.yaml"), `
name: fetch
version: 0.1.0
entry: bin/fetch
`)
	writeFile(t, filepath.Join(src, "bin", "fetch"), "#!/bin/true\n")

	s := New(t.TempDir())
	img, err := s.Put(src)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if img.Ref().Name != "fetch" || img.Ref().Version != "0.1.0" {
		t.Fatalf("ref: %+v", img.Ref())
	}
	if _, err := os.Stat(filepath.Join(img.Dir, "bin", "fetch")); err != nil {
		t.Fatalf("copied file missing: %v", err)
	}

	got, err := s.Get(image.Ref{Name: "fetch", Version: "0.1.0"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Manifest.Entry != "bin/fetch" {
		t.Fatalf("unexpected entry: %q", got.Manifest.Entry)
	}

	refs, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("expected 1 image, got %d", len(refs))
	}
}

func TestGetMissing(t *testing.T) {
	s := New(t.TempDir())
	if _, err := s.Get(image.Ref{Name: "x", Version: "1"}); err == nil {
		t.Fatal("expected error for missing image")
	}
}

func TestPutOverwrites(t *testing.T) {
	src1 := t.TempDir()
	writeFile(t, filepath.Join(src1, "agent.yaml"), `
name: x
version: 1
entry: old
`)
	writeFile(t, filepath.Join(src1, "old"), "old")

	src2 := t.TempDir()
	writeFile(t, filepath.Join(src2, "agent.yaml"), `
name: x
version: 1
entry: new
`)
	writeFile(t, filepath.Join(src2, "new"), "new")

	s := New(t.TempDir())
	if _, err := s.Put(src1); err != nil {
		t.Fatal(err)
	}
	img, err := s.Put(src2)
	if err != nil {
		t.Fatal(err)
	}
	if img.Manifest.Entry != "new" {
		t.Fatal("second Put did not overwrite manifest")
	}
	// old file should be gone
	if _, err := os.Stat(filepath.Join(img.Dir, "old")); !os.IsNotExist(err) {
		t.Fatal("old file should have been removed by overwrite")
	}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
