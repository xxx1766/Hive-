package remote

import "testing"

func TestParseRef_GithubScheme(t *testing.T) {
	r, err := ParseRef("github://xxx1766/Hive-/registry/agents/brief")
	if err != nil {
		t.Fatal(err)
	}
	if r.Owner != "xxx1766" || r.Repo != "Hive-" || r.Path != "registry/agents/brief" || r.Ref != "main" {
		t.Fatalf("bad parse: %+v", r)
	}
}

func TestParseRef_GithubScheme_WithRef(t *testing.T) {
	r, err := ParseRef("github://xxx1766/Hive-/registry/agents/brief@v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if r.Ref != "v0.1.0" {
		t.Fatalf("ref: %q", r.Ref)
	}
	if r.Path != "registry/agents/brief" {
		t.Fatalf("path: %q", r.Path)
	}
}

func TestParseRef_GithubScheme_NoPath(t *testing.T) {
	r, err := ParseRef("github://xxx1766/Hive-@abc123")
	if err != nil {
		t.Fatal(err)
	}
	if r.Path != "" || r.Ref != "abc123" {
		t.Fatalf("bad parse: %+v", r)
	}
}

func TestParseRef_HTTPS_TreePath(t *testing.T) {
	r, err := ParseRef("https://github.com/xxx1766/Hive-/tree/main/registry/agents/brief")
	if err != nil {
		t.Fatal(err)
	}
	if r.Owner != "xxx1766" || r.Repo != "Hive-" || r.Path != "registry/agents/brief" || r.Ref != "main" {
		t.Fatalf("bad parse: %+v", r)
	}
}

func TestParseRef_HTTPS_BlobPath(t *testing.T) {
	r, err := ParseRef("https://github.com/xxx1766/Hive-/blob/v1.0/registry/agents/brief/agent.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if r.Ref != "v1.0" || r.Path != "registry/agents/brief/agent.yaml" {
		t.Fatalf("bad parse: %+v", r)
	}
}

func TestParseRef_HTTPS_NoTreeBlob(t *testing.T) {
	// /owner/repo alone — no tree/blob segment.
	r, err := ParseRef("https://github.com/xxx1766/Hive-")
	if err != nil {
		t.Fatal(err)
	}
	if r.Path != "" || r.Ref != "main" {
		t.Fatalf("expected path=main root, got %+v", r)
	}
}

func TestParseRef_ShortForm(t *testing.T) {
	r, err := ParseRef("xxx1766/Hive-#registry/agents/brief")
	if err != nil {
		t.Fatal(err)
	}
	if r.Owner != "xxx1766" || r.Repo != "Hive-" || r.Path != "registry/agents/brief" || r.Ref != "main" {
		t.Fatalf("bad parse: %+v", r)
	}
}

func TestParseRef_ShortForm_WithRef(t *testing.T) {
	r, err := ParseRef("xxx1766/Hive-#registry/agents/brief@v0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if r.Ref != "v0.1.0" {
		t.Fatalf("ref: %q", r.Ref)
	}
}

func TestParseRef_Rejects(t *testing.T) {
	bads := []string{
		"",
		"just-a-string",
		"brief:0.1.0",                   // local image ref, not remote
		"github://",                     // missing owner/repo
		"github://only-owner",           // only owner
		"https://gitlab.com/foo/bar",    // wrong host
		"#path",                         // no owner/repo before #
		"/Hive-#path",                   // missing owner
	}
	for _, s := range bads {
		if _, err := ParseRef(s); err == nil {
			t.Errorf("expected error for %q", s)
		}
	}
}

func TestLooksRemote(t *testing.T) {
	remotes := []string{
		"github://x/y",
		"github://x/y/path@ref",
		"https://github.com/x/y",
		"https://github.com/x/y/tree/main/path",
		"x/y#path",
		"x/y#path@ref",
	}
	locals := []string{
		"brief:0.1.0",
		"echo:latest",
		"some-name:1.2.3",
		"plain-string",
		"",
	}
	for _, s := range remotes {
		if !LooksRemote(s) {
			t.Errorf("LooksRemote(%q) = false, want true", s)
		}
	}
	for _, s := range locals {
		if LooksRemote(s) {
			t.Errorf("LooksRemote(%q) = true, want false", s)
		}
	}
}

func TestRawURL(t *testing.T) {
	r := &Ref{Owner: "xxx1766", Repo: "Hive-", Path: "registry/agents/brief", Ref: "main"}
	want := "https://raw.githubusercontent.com/xxx1766/Hive-/main/registry/agents/brief/agent.yaml"
	if got := r.RawURL("agent.yaml"); got != want {
		t.Errorf("RawURL:\n got %s\nwant %s", got, want)
	}
}

func TestRawURL_EmptyPath(t *testing.T) {
	// path = "" (repo root): no double slash.
	r := &Ref{Owner: "x", Repo: "y", Path: "", Ref: "main"}
	want := "https://raw.githubusercontent.com/x/y/main/README.md"
	if got := r.RawURL("README.md"); got != want {
		t.Errorf("RawURL:\n got %s\nwant %s", got, want)
	}
}

func TestRefString_OmitsDefaultRef(t *testing.T) {
	r := &Ref{Owner: "x", Repo: "y", Path: "p", Ref: "main"}
	if got := r.String(); got != "github://x/y/p" {
		t.Errorf("String() = %q", got)
	}
	r.Ref = "v1"
	if got := r.String(); got != "github://x/y/p@v1" {
		t.Errorf("String() with non-default ref = %q", got)
	}
}
