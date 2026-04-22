package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/anne-x/hive/internal/ipc"
)

// cmdPull explicitly fetches a remote Agent into the local store.
// Usage: hive pull <url>
// Equivalent to the auto-pull step of `hive hire <room> <url>`.
func cmdPull(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: hive pull <url>")
		fmt.Fprintln(os.Stderr, "  <url>: github://owner/repo/path[@ref]")
		fmt.Fprintln(os.Stderr, "       | https://github.com/owner/repo/tree/ref/path")
		fmt.Fprintln(os.Stderr, "       | owner/repo#path[@ref]")
		os.Exit(2)
	}
	c := mustDial(ctx)
	defer c.Close()
	raw, err := c.Call(ctx, ipc.MethodImagePull, ipc.ImagePullParams{URL: args[0]})
	if err != nil {
		fmt.Fprintf(os.Stderr, "pull: %v\n", err)
		os.Exit(1)
	}
	var r ipc.ImagePullResult
	_ = json.Unmarshal(raw, &r)
	fmt.Printf("pulled %s → %s\n", r.Image, r.Path)
}

// pullIfRemote, when given a remote ref, returns the local name:version
// string (after daemon-side pull). When given a plain name:version, it
// returns the input unchanged. Used by `hive hire` and `hive up` so the
// rest of their code paths only deal with local refs.
func pullIfRemote(ctx context.Context, c *ipcClient, refStr string) (string, error) {
	if !looksRemoteRef(refStr) {
		return refStr, nil
	}
	raw, err := c.Call(ctx, ipc.MethodImagePull, ipc.ImagePullParams{URL: refStr})
	if err != nil {
		return "", fmt.Errorf("pull %s: %w", refStr, err)
	}
	var r ipc.ImagePullResult
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", err
	}
	return r.Image.Name + ":" + r.Image.Version, nil
}

// Thin shims so cmd/hive/ doesn't pick up a direct dep on internal/remote
// (keeps the import graph shallow — the daemon is the one that does the
// real parsing and fetching). Package-level ipc.Client can't be aliased
// cleanly in Go without generics, hence the explicit type.
type ipcClient = ipc.Client

func looksRemoteRef(s string) bool {
	// Must stay in sync with internal/remote.LooksRemote. Duplicating the
	// predicate here avoids the CLI binary pulling in gopkg.in/yaml.v3 etc.
	// via internal/remote (which needs image+store).
	if len(s) == 0 {
		return false
	}
	if len(s) >= 9 && s[:9] == "github://" {
		return true
	}
	if len(s) >= 19 && s[:19] == "https://github.com/" {
		return true
	}
	if len(s) >= 18 && s[:18] == "http://github.com/" {
		return true
	}
	hasHash, hasSlash, hasColon := false, false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '#':
			hasHash = true
		case '/':
			hasSlash = true
		case ':':
			hasColon = true
		}
	}
	return hasHash && hasSlash && !hasColon
}
