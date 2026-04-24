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

func TestLoadVolumes_Happy(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "h.yaml")
	body := `
room: demo
agents:
  - image: fetch:0.1.0
    volumes:
      - name: kb
        mode: ro
        mountpoint: /shared/kb
      - name: cache
        mode: rw
        mountpoint: /shared/cache
`
	os.WriteFile(p, []byte(body), 0o644)
	hf, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(hf.Agents[0].Volumes) != 2 {
		t.Fatalf("volumes parsed: %+v", hf.Agents[0].Volumes)
	}
}

func TestLoadVolumes_Rejects(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing name", `
room: d
agents:
  - image: x:1
    volumes: [{name: "", mountpoint: /x}]
`},
		{"relative mountpoint", `
room: d
agents:
  - image: x:1
    volumes: [{name: v, mountpoint: relative/path}]
`},
		{"dotdot mountpoint", `
room: d
agents:
  - image: x:1
    volumes: [{name: v, mountpoint: /shared/../etc}]
`},
		{"invalid mode", `
room: d
agents:
  - image: x:1
    volumes: [{name: v, mountpoint: /x, mode: weird}]
`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "h.yaml")
			os.WriteFile(p, []byte(tc.body), 0o644)
			if _, err := Load(p); err == nil {
				t.Fatalf("expected rejection for %s", tc.name)
			}
		})
	}
}

func TestLoadVolumes_ModeDefaultsRO(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "h.yaml")
	os.WriteFile(p, []byte(`
room: d
agents:
  - image: x:1
    volumes: [{name: v, mountpoint: /shared}]
`), 0o644)
	hf, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if hf.Agents[0].Volumes[0].Mode != "ro" {
		t.Fatalf("expected ro default, got %q", hf.Agents[0].Volumes[0].Mode)
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
