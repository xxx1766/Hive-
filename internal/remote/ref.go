// Package remote resolves and fetches Agent / Hivefile source from
// GitHub-hosted registries. MVP only supports github.com; the parser is
// factored so adding gitlab / gitee later is additive.
package remote

import (
	"fmt"
	"net/url"
	"strings"
)

// defaultRef is used when the user omits @ref. "main" is GitHub's
// default branch name since 2020; if repos still use "master" the
// user must spell @master explicitly.
const defaultRef = "main"

// Ref points at a directory (or single file) inside a GitHub repository
// at a given commit/branch/tag.
type Ref struct {
	Host  string // "github.com" (constant in MVP; future-proof for gitlab.com / gitee.com)
	Owner string
	Repo  string
	Path  string // directory or file path inside the repo; no leading or trailing slash
	Ref   string // branch / tag / commit sha; defaults to "main"
}

// ParseRef accepts any of the three user-facing forms:
//
//	github://owner/repo/path[@ref]                                 (scheme form)
//	https://github.com/owner/repo/{tree,blob}/ref/path              (browser URL)
//	owner/repo#path[@ref]                                           (short form, go-get-ish)
//
// Returns (*Ref, nil) on success, (nil, err) with a descriptive message otherwise.
func ParseRef(s string) (*Ref, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "github://"):
		return parseGithubScheme(s)
	case strings.HasPrefix(s, "https://github.com/"), strings.HasPrefix(s, "http://github.com/"):
		return parseGithubHTTPS(s)
	case strings.Contains(s, "#"):
		return parseShortForm(s)
	default:
		return nil, fmt.Errorf("remote: unrecognised ref %q (want github://..., https://github.com/..., or owner/repo#path)", s)
	}
}

// LooksRemote is a cheap pre-check for CLI auto-detection.
// Keeps false-positives narrow so a plain "name:version" never matches.
func LooksRemote(s string) bool {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "github://"):
		return true
	case strings.HasPrefix(s, "https://github.com/"), strings.HasPrefix(s, "http://github.com/"):
		return true
	case strings.Contains(s, "#") && strings.Contains(s, "/") && !strings.Contains(s, ":"):
		// owner/repo#path — require '/' and forbid ':' so we don't swallow name:version.
		return true
	}
	return false
}

// ── form-specific parsers ─────────────────────────────────────────────────

func parseGithubScheme(s string) (*Ref, error) {
	rest := strings.TrimPrefix(s, "github://")
	ref, rest := splitRef(rest)
	parts := strings.Split(strings.Trim(rest, "/"), "/")
	if len(parts) < 2 {
		return nil, fmt.Errorf("remote: github:// needs at least owner/repo, got %q", s)
	}
	return mkRef("github.com", parts[0], parts[1], strings.Join(parts[2:], "/"), ref)
}

func parseGithubHTTPS(s string) (*Ref, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, fmt.Errorf("remote: parse %q: %w", s, err)
	}
	p := strings.Trim(u.Path, "/")
	parts := strings.Split(p, "/")
	if len(parts) < 2 {
		return nil, fmt.Errorf("remote: URL missing owner/repo: %q", s)
	}
	owner, repo := parts[0], parts[1]
	var ref, subpath string
	// /owner/repo → root of main
	// /owner/repo/tree/<ref>/<path...>
	// /owner/repo/blob/<ref>/<path...>
	if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob") {
		ref = parts[3]
		subpath = strings.Join(parts[4:], "/")
	}
	return mkRef("github.com", owner, repo, subpath, ref)
}

func parseShortForm(s string) (*Ref, error) {
	hashAt := strings.Index(s, "#")
	if hashAt < 0 {
		return nil, fmt.Errorf("remote: short form needs '#'")
	}
	ownerRepo := s[:hashAt]
	tail := s[hashAt+1:]

	parts := strings.SplitN(ownerRepo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("remote: short form left side must be owner/repo, got %q", ownerRepo)
	}
	ref, path := splitRef(tail)
	return mkRef("github.com", parts[0], parts[1], path, ref)
}

// splitRef splits "path@ref" into (ref, path). If no '@', returns ("", s).
// The LAST '@' wins so paths with '@' (rare) don't break — and '@' in git
// refs is illegal anyway, so no real collision.
func splitRef(s string) (ref, rest string) {
	if at := strings.LastIndex(s, "@"); at >= 0 {
		return s[at+1:], s[:at]
	}
	return "", s
}

func mkRef(host, owner, repo, subpath, ref string) (*Ref, error) {
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("remote: owner and repo are both required")
	}
	if ref == "" {
		ref = defaultRef
	}
	return &Ref{
		Host:  host,
		Owner: owner,
		Repo:  repo,
		Path:  strings.Trim(subpath, "/"),
		Ref:   ref,
	}, nil
}

// ── URL rendering ─────────────────────────────────────────────────────────

// RawURL returns the raw.githubusercontent.com URL for a file relative to
// Ref.Path. Pass "" to get the URL for an empty "bare" path (rarely useful).
func (r *Ref) RawURL(file string) string {
	file = strings.TrimLeft(file, "/")
	p := r.Path
	if p != "" && file != "" {
		p += "/"
	}
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s%s",
		r.Owner, r.Repo, r.Ref, p, file)
}

// String is the canonical github:// form; useful for log lines and tests.
func (r *Ref) String() string {
	base := fmt.Sprintf("github://%s/%s", r.Owner, r.Repo)
	if r.Path != "" {
		base += "/" + r.Path
	}
	if r.Ref != "" && r.Ref != defaultRef {
		base += "@" + r.Ref
	}
	return base
}
