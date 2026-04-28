package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/anne-x/hive/internal/ipc"
	"github.com/anne-x/hive/internal/version"
)

// installRecord is the breadcrumb scripts/install.sh drops at
// $HIVE_STATE/install.json so `hive update` can find the source tree
// later. Keep the field set in sync with install.sh.
type installRecord struct {
	SourceDir   string `json:"source_dir"`
	Prefix      string `json:"prefix"`
	InstalledAt string `json:"installed_at"`
}

// cmdUpdate pulls the latest hive source, rebuilds the four binaries,
// and reinstalls them via scripts/install.sh. Distribution is source-only
// (no GitHub Releases), so "update" = git pull + make build + install.
//
// Daemon handling: install -m 755 is atomic on Linux, so replacing
// /usr/local/bin/hived under a running daemon is safe — the old process
// keeps its mmap'd binary and the next `hived` invocation picks up the
// new file. We only print a restart hint, never try to manage hived.
func cmdUpdate(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("update", args) {
		return
	}

	var (
		sourceDir string
		ref       string
		check     bool
		force     bool
		prefix    string
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--check":
			check = true
		case a == "--force":
			force = true
		case a == "--source-dir" && i+1 < len(args):
			sourceDir = args[i+1]
			i++
		case strings.HasPrefix(a, "--source-dir="):
			sourceDir = strings.TrimPrefix(a, "--source-dir=")
		case a == "--ref" && i+1 < len(args):
			ref = args[i+1]
			i++
		case strings.HasPrefix(a, "--ref="):
			ref = strings.TrimPrefix(a, "--ref=")
		case a == "--prefix" && i+1 < len(args):
			prefix = args[i+1]
			i++
		case strings.HasPrefix(a, "--prefix="):
			prefix = strings.TrimPrefix(a, "--prefix=")
		default:
			fmt.Fprintf(os.Stderr, "update: unknown flag %q\n", a)
			printCommandHelp("update", os.Stderr)
			os.Exit(2)
		}
	}

	src, err := resolveSourceDir(sourceDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: %v\n", err)
		os.Exit(1)
	}

	// Sanity: must be a git working tree with a Makefile + scripts/install.sh.
	if _, err := os.Stat(filepath.Join(src, ".git")); err != nil {
		fmt.Fprintf(os.Stderr, "update: %s is not a git working tree\n", src)
		os.Exit(1)
	}
	if _, err := os.Stat(filepath.Join(src, "Makefile")); err != nil {
		fmt.Fprintf(os.Stderr, "update: %s has no Makefile (not a hive source tree?)\n", src)
		os.Exit(1)
	}
	if _, err := os.Stat(filepath.Join(src, "scripts", "install.sh")); err != nil {
		fmt.Fprintf(os.Stderr, "update: %s/scripts/install.sh missing\n", src)
		os.Exit(1)
	}

	fmt.Printf("source: %s\n", src)
	fmt.Printf("currently installed: %s\n", version.Version)

	// Switch to --ref if requested. Done before fetch so the fetch updates
	// the right branch.
	if ref != "" {
		if err := runIn(ctx, src, "git", "checkout", ref); err != nil {
			fmt.Fprintf(os.Stderr, "update: git checkout %s: %v\n", ref, err)
			os.Exit(1)
		}
	}

	// Fetch and compare.
	if err := runIn(ctx, src, "git", "fetch", "--quiet"); err != nil {
		fmt.Fprintf(os.Stderr, "update: git fetch: %v\n", err)
		os.Exit(1)
	}
	localSHA, err := captureIn(ctx, src, "git", "rev-parse", "--short", "HEAD")
	if err != nil {
		fmt.Fprintf(os.Stderr, "update: git rev-parse HEAD: %v\n", err)
		os.Exit(1)
	}
	upstreamSHA, err := captureIn(ctx, src, "git", "rev-parse", "--short", "@{u}")
	if err != nil {
		// No upstream configured — surface clearly; user needs to set it
		// or pass --ref.
		fmt.Fprintf(os.Stderr, "update: current branch has no upstream — set one with `git branch --set-upstream-to=...` or pass --ref\n")
		os.Exit(1)
	}
	fmt.Printf("local:    %s\n", localSHA)
	fmt.Printf("upstream: %s\n", upstreamSHA)

	if check {
		if localSHA == upstreamSHA {
			fmt.Println("up to date.")
		} else {
			fmt.Println("update available — run `hive update` to apply.")
		}
		return
	}

	if localSHA == upstreamSHA && !force {
		fmt.Println("already up to date.")
		return
	}

	// Pull. --ff-only refuses if upstream diverged from local — the right
	// default to protect any in-progress work in the source tree. User can
	// resolve manually and rerun.
	if localSHA != upstreamSHA {
		if err := runIn(ctx, src, "git", "pull", "--ff-only"); err != nil {
			fmt.Fprintf(os.Stderr, "update: git pull --ff-only: %v\n", err)
			fmt.Fprintln(os.Stderr, "       (resolve any local changes manually, then rerun)")
			os.Exit(1)
		}
	}

	// Rebuild + reinstall. Two phases keep stdout streaming so the user
	// sees progress (Go compiles can take a while on first run).
	fmt.Println("building…")
	if err := runIn(ctx, src, "make", "build"); err != nil {
		fmt.Fprintf(os.Stderr, "update: make build: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("installing…")
	installEnv := os.Environ()
	if prefix != "" {
		installEnv = append(installEnv, "PREFIX="+prefix)
	}
	if err := runInEnv(ctx, src, installEnv, "./scripts/install.sh", "--skip-build"); err != nil {
		fmt.Fprintf(os.Stderr, "update: install.sh: %v\n", err)
		os.Exit(1)
	}

	newSHA, _ := captureIn(ctx, src, "git", "rev-parse", "--short", "HEAD")
	fmt.Printf("\nupdated %s → %s\n", localSHA, newSHA)

	// Daemon restart hint. Probe the socket; if we can dial, hived is
	// running the previous binary in memory.
	if c, err := ipc.Dial(ctx, ipc.SocketPath()); err == nil {
		_ = c.Close()
		fmt.Println()
		fmt.Println("note: hived is still running the previous version. To pick up the update:")
		fmt.Println("        sudo killall hived && sudo hived &")
	}
}

// resolveSourceDir walks the resolution order documented in the plan:
// 1) explicit flag → 2) $HIVE_STATE/install.json → 3) walk up from the
// running binary looking for a hive source layout.
func resolveSourceDir(flagVal string) (string, error) {
	if flagVal != "" {
		abs, err := filepath.Abs(flagVal)
		if err != nil {
			return "", err
		}
		return abs, nil
	}

	// Breadcrumb left by scripts/install.sh.
	recPath := filepath.Join(ipc.StateRoot(), "install.json")
	if data, err := os.ReadFile(recPath); err == nil {
		var rec installRecord
		if err := json.Unmarshal(data, &rec); err == nil && rec.SourceDir != "" {
			if _, err := os.Stat(rec.SourceDir); err == nil {
				return rec.SourceDir, nil
			}
		}
	}

	// Dev-tree fallback: walk up from the running hive binary looking for
	// a directory that contains both .git/ and Makefile. Handles
	// `./bin/hive update` from the source checkout.
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for i := 0; i < 16; i++ { // bound the walk
			if isHiveSourceTree(dir) {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}

	return "", errors.New("could not locate hive source tree — re-run scripts/install.sh from the source dir, or pass --source-dir")
}

func isHiveSourceTree(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "Makefile")); err != nil {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "scripts", "install.sh")); err != nil {
		return false
	}
	return true
}

// runIn streams a child process's stdout/stderr through to ours.
func runIn(ctx context.Context, dir, name string, args ...string) error {
	return runInEnv(ctx, dir, nil, name, args...)
}

func runInEnv(ctx context.Context, dir string, env []string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if env != nil {
		cmd.Env = env
	}
	return cmd.Run()
}

// captureIn runs a command and returns trimmed stdout. Used for short
// metadata reads like `git rev-parse`.
func captureIn(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if stderr.Len() > 0 {
			return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

