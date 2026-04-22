package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anne-x/hive/internal/hivefile"
	"github.com/anne-x/hive/internal/image"
	"github.com/anne-x/hive/internal/ipc"
)

// cmdUp: `hive up <hivefile-or-url>` — init a Room and hire all declared Agents.
// Prints the Room ID on success so callers can pipe it into `hive run`.
//
// The hivefile source can be a local path OR a remote ref (any of the
// three accepted forms). Inside the Hivefile, each agent's `image:`
// field can likewise be local (name:version) or remote — remote ones
// are pulled via the daemon's image/pull before hire.
func cmdUp(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("up", args) {
		return
	}
	if len(args) < 1 {
		printCommandHelp("up", os.Stderr)
		os.Exit(2)
	}
	src := args[0]

	hfPath := src
	if looksRemoteRef(src) {
		// Download Hivefile.yaml to a temp file, then fall through.
		tmp, err := fetchHivefileToTemp(ctx, src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "up: fetch hivefile: %v\n", err)
			os.Exit(1)
		}
		defer os.Remove(tmp)
		hfPath = tmp
	}

	hf, err := hivefile.Load(hfPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "up: %v\n", err)
		os.Exit(1)
	}

	c := mustDial(ctx)
	defer c.Close()

	// 1. Init Room.
	raw, err := c.Call(ctx, ipc.MethodRoomInit, ipc.RoomInitParams{Name: hf.Room})
	if err != nil {
		fmt.Fprintf(os.Stderr, "up/init: %v\n", err)
		os.Exit(1)
	}
	var init ipc.RoomInitResult
	_ = json.Unmarshal(raw, &init)
	fmt.Fprintf(os.Stderr, "  room %s created\n", init.RoomID)

	// 2. Hire each declared Agent (pulling remotes on the fly).
	for _, a := range hf.Agents {
		localRef, err := pullIfRemote(ctx, c, a.Image)
		if err != nil {
			fmt.Fprintf(os.Stderr, "up/pull %s: %v\n", a.Image, err)
			os.Exit(1)
		}
		ref, _ := image.ParseRef(localRef)

		// If the Hivefile entry set `quota:`, serialise it through to the
		// daemon as an override on the Rank default. Partial overrides
		// are the norm — unset keys inherit from the Rank.
		var quotaRaw json.RawMessage
		if len(a.Quota) > 0 {
			b, err := json.Marshal(a.Quota)
			if err != nil {
				fmt.Fprintf(os.Stderr, "up/quota %s: %v\n", a.Image, err)
				os.Exit(1)
			}
			quotaRaw = b
		}

		_, err = c.Call(ctx, ipc.MethodAgentHire, ipc.AgentHireParams{
			RoomID:     init.RoomID,
			Image:      ipc.ImageRef{Name: ref.Name, Version: ref.Version},
			RankName:   a.Rank,
			QuotaOverr: quotaRaw,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "up/hire %s: %v\n", a.Image, err)
			os.Exit(1)
		}
		display := a.Image
		if display != localRef {
			display = fmt.Sprintf("%s (= %s)", a.Image, localRef)
		}
		fmt.Fprintf(os.Stderr, "  hired %s\n", display)
	}

	// 3. Print RoomID to stdout so shell scripts can capture it.
	fmt.Println(init.RoomID)
}

// fetchHivefileToTemp turns a remote ref into a local temp file path
// containing the fetched Hivefile.yaml. We do this in the CLI (not the
// daemon) because Hivefile.Load wants an os-readable path — the daemon
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

	tmp, err := os.CreateTemp("", "hive-up-*.yaml")
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
