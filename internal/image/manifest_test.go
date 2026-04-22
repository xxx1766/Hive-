package image

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseRef(t *testing.T) {
	cases := []struct {
		in      string
		wantN   string
		wantV   string
		wantErr bool
	}{
		{"fetch:0.1.0", "fetch", "0.1.0", false},
		{"hive/fetch:0.1.0", "hive/fetch", "0.1.0", false},
		{"no-version", "", "", true},
		{":0.1.0", "", "", true},
		{"fetch:", "", "", true},
	}
	for _, tc := range cases {
		r, err := ParseRef(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("%q: expected error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected err %v", tc.in, err)
			continue
		}
		if r.Name != tc.wantN || r.Version != tc.wantV {
			t.Errorf("%q: got %q:%q want %q:%q", tc.in, r.Name, r.Version, tc.wantN, tc.wantV)
		}
	}
}

func TestLoadManifestValid(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "hive.yaml"), `
name: fetch
version: 0.1.0
entry: bin/fetch
rank: staff
`)
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Name != "fetch" || m.Version != "0.1.0" || m.Entry != "bin/fetch" || m.Rank != "staff" {
		t.Errorf("unexpected manifest: %+v", m)
	}
}

func TestLoadManifestDefaultsRank(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "hive.yaml"), `
name: a
version: 1
entry: x
`)
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Rank != "intern" {
		t.Fatalf("rank default: got %q want intern", m.Rank)
	}
}

func TestLoadManifestMissingName(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "hive.yaml"), `
version: 1
entry: x
`)
	if _, err := LoadManifest(dir); err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestLoadManifestRejectsDotDotEntry(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "hive.yaml"), `
name: a
version: 1
entry: ../../../etc/passwd
`)
	if _, err := LoadManifest(dir); err == nil {
		t.Fatal("expected path-escape rejection")
	}
}

func TestLoadManifestDefaultsKindBinary(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "hive.yaml"), `
name: a
version: 1
entry: bin/a
`)
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Kind != KindBinary {
		t.Fatalf("kind default: got %q want %q", m.Kind, KindBinary)
	}
}

func TestLoadManifestKindSkillRequiresSkillField(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "hive.yaml"), `
name: a
version: 1
kind: skill
`)
	if _, err := LoadManifest(dir); err == nil {
		t.Fatal("expected rejection when kind=skill but no skill field")
	}
}

func TestLoadManifestKindSkillHappy(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "hive.yaml"), `
name: brief
version: 0.1.0
kind: skill
skill: SKILL.md
model: gpt-4o-mini
tools: [net, fs]
rank: staff
`)
	m, err := LoadManifest(dir)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.Kind != KindSkill || m.Skill != "SKILL.md" || m.Model != "gpt-4o-mini" {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if len(m.Tools) != 2 {
		t.Fatalf("tools parse: %+v", m.Tools)
	}
}

func TestLoadManifestKindSkillRejectsDotDot(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "hive.yaml"), `
name: bad
version: 1
kind: skill
skill: ../../etc/shadow
`)
	if _, err := LoadManifest(dir); err == nil {
		t.Fatal("expected path-escape rejection")
	}
}

func TestLoadManifestUnknownKind(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "hive.yaml"), `
name: a
version: 1
kind: sorcery
`)
	if _, err := LoadManifest(dir); err == nil {
		t.Fatal("expected unknown-kind rejection")
	}
}

func TestLoadManifestKindJSONNotYetImplemented(t *testing.T) {
	dir := t.TempDir()
	writeYAML(t, filepath.Join(dir, "hive.yaml"), `
name: a
version: 1
kind: json
`)
	if _, err := LoadManifest(dir); err == nil {
		t.Fatal("expected json not-yet-implemented rejection")
	}
}

func writeYAML(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
