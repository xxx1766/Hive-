package volume

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCreate_Happy(t *testing.T) {
	m := newManager(t)
	v, err := m.Create("llm-cache")
	if err != nil {
		t.Fatal(err)
	}
	if v.Name != "llm-cache" {
		t.Fatalf("name: %q", v.Name)
	}
	if _, err := os.Stat(v.MemoryDir()); err != nil {
		t.Fatalf("memory dir should exist: %v", err)
	}
}

func TestCreate_DuplicateRejected(t *testing.T) {
	m := newManager(t)
	if _, err := m.Create("x"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("x"); err == nil {
		t.Fatal("expected duplicate rejection")
	}
}

func TestCreate_InvalidName(t *testing.T) {
	m := newManager(t)
	bad := []string{"", "has space", "has/slash", "has..dots", strings.Repeat("a", 65), "dot."}
	for _, name := range bad {
		if _, err := m.Create(name); err == nil {
			t.Errorf("expected rejection for %q", name)
		}
	}
}

func TestGet_Missing(t *testing.T) {
	m := newManager(t)
	if _, err := m.Get("nope"); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestList_SortedAndFiltersInvalid(t *testing.T) {
	m := newManager(t)
	_, _ = m.Create("zeta")
	_, _ = m.Create("alpha")
	_, _ = m.Create("mid")
	// a rogue directory that doesn't match nameRe should be ignored
	_ = os.MkdirAll(filepath.Join(m.Root, "not a volume"), 0o755)

	vols, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(vols) != 3 {
		t.Fatalf("want 3 vols, got %d (%+v)", len(vols), vols)
	}
	if vols[0].Name != "alpha" || vols[2].Name != "zeta" {
		t.Fatalf("not sorted: %+v", vols)
	}
}

func TestRemove_Idempotent(t *testing.T) {
	m := newManager(t)
	_, _ = m.Create("gone")
	if err := m.Remove("gone"); err != nil {
		t.Fatal(err)
	}
	// Removing again must NOT error.
	if err := m.Remove("gone"); err != nil {
		t.Fatalf("second remove should be no-op: %v", err)
	}
	if _, err := m.Get("gone"); err == nil {
		t.Fatal("removed volume should not Get")
	}
}
