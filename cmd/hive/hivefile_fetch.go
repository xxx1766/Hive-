package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// fetchHivefileToTemp turns a remote ref into a local temp file path
// containing the fetched Hivefile.yaml. We do this in the CLI (not the
// daemon) because hivefile.Load wants an os-readable path — the daemon
// doesn't need to own this transport.
func fetchHivefileToTemp(ctx context.Context, ref string) (string, error) {
	rawURL, err := hivefileRawURL(ref)
	if err != nil {
		return "", err
	}
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, "GET", rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "hive-cli")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("GET %s: %s", rawURL, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp("", "hive-hire-*.yaml")
	if err != nil {
		return "", err
	}
	defer tmp.Close()
	if _, err := tmp.Write(body); err != nil {
		return "", err
	}
	return tmp.Name(), nil
}

// hivefileRawURL parses a remote ref and returns the raw URL for Hivefile.yaml.
// We duplicate a minimal parse here (instead of importing internal/remote)
// to keep CLI binary lean — the exhaustive parser lives daemon-side.
func hivefileRawURL(ref string) (string, error) {
	owner, repo, path, gitref, err := splitRemoteRef(ref)
	if err != nil {
		return "", err
	}
	sep := "/"
	if path == "" {
		sep = ""
	}
	return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/%s/%s%sHivefile.yaml",
		owner, repo, gitref, path, sep), nil
}

// splitRemoteRef: three-form parse returning the same fields as
// internal/remote.Ref. Kept independent so cmd/hive stays small.
func splitRemoteRef(s string) (owner, repo, path, gitref string, err error) {
	gitref = "main"
	switch {
	case strings.HasPrefix(s, "github://"):
		rest := strings.TrimPrefix(s, "github://")
		if at := strings.LastIndex(rest, "@"); at >= 0 {
			gitref = rest[at+1:]
			rest = rest[:at]
		}
		parts := strings.Split(strings.Trim(rest, "/"), "/")
		if len(parts) < 2 {
			return "", "", "", "", fmt.Errorf("github://: need owner/repo")
		}
		owner, repo = parts[0], parts[1]
		path = strings.Join(parts[2:], "/")

	case strings.HasPrefix(s, "https://github.com/"), strings.HasPrefix(s, "http://github.com/"):
		rest := s[strings.Index(s, "github.com/")+len("github.com/"):]
		parts := strings.Split(strings.Trim(rest, "/"), "/")
		if len(parts) < 2 {
			return "", "", "", "", fmt.Errorf("https URL: need owner/repo")
		}
		owner, repo = parts[0], parts[1]
		if len(parts) >= 4 && (parts[2] == "tree" || parts[2] == "blob") {
			gitref = parts[3]
			path = strings.Join(parts[4:], "/")
		}

	case strings.Contains(s, "#"):
		hashAt := strings.Index(s, "#")
		ownerRepo := s[:hashAt]
		tail := s[hashAt+1:]
		parts := strings.SplitN(ownerRepo, "/", 2)
		if len(parts) != 2 {
			return "", "", "", "", fmt.Errorf("short form: need owner/repo")
		}
		owner, repo = parts[0], parts[1]
		if at := strings.LastIndex(tail, "@"); at >= 0 {
			gitref = tail[at+1:]
			tail = tail[:at]
		}
		path = tail

	default:
		return "", "", "", "", fmt.Errorf("unrecognised ref: %s", s)
	}
	path = strings.Trim(path, "/")
	return
}
