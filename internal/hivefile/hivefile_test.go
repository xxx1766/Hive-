package hivefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadValid(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "h.yaml")
	body := `
room: demo
entry: upper
agents:
  - image: fetch:0.1.0
    rank: intern
  - image: upper:0.1.0
    rank: staff
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	hf, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(hf.Agents) != 2 {
		t.Fatalf("agents: %d", len(hf.Agents))
	}
	if hf.Room != "demo" {
		t.Fatalf("room: %q", hf.Room)
	}
}

func TestLoadRejectsDuplicate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "h.yaml")
	body := `
room: demo
agents:
  - image: fetch:0.1.0
  - image: fetch:0.2.0
`
	os.WriteFile(p, []byte(body), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("expected duplicate image name rejection")
	}
}

func TestLoadRejectsEntryNotInAgents(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "h.yaml")
	body := `
room: demo
entry: ghost
agents:
  - image: fetch:0.1.0
`
	os.WriteFile(p, []byte(body), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("expected entry-not-hired rejection")
	}
}

func TestLoadRejectsEmpty(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "h.yaml")
	os.WriteFile(p, []byte("room: x\n"), 0o644)
	if _, err := Load(p); err == nil {
		t.Fatal("expected empty-agents rejection")
	}
}
