package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/anne-x/hive/internal/ipc"
	"github.com/anne-x/hive/internal/version"
)

func main() {
	if len(os.Args) < 2 {
		printTopHelp(os.Stderr)
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	// Top-level help forms.
	switch cmd {
	case "help":
		if len(args) == 0 {
			printTopHelp(os.Stdout)
			return
		}
		if !printCommandHelp(args[0], os.Stdout) {
			fmt.Fprintf(os.Stderr, "hive: unknown command %q\n", args[0])
			printTopHelp(os.Stderr)
			os.Exit(2)
		}
		return
	case "--help", "-h":
		printTopHelp(os.Stdout)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch cmd {
	case "version", "--version", "-v":
		if maybeHandleHelpFlag("version", args) {
			return
		}
		cmdVersion(ctx)
	case "ping":
		if maybeHandleHelpFlag("ping", args) {
			return
		}
		cmdPing(ctx)
	default:
		// M2+ commands dispatch through dispatchCmd. dispatchCmd's
		// handlers each call maybeHandleHelpFlag themselves so they can
		// return early before any network calls.
		if !dispatchCmd(ctx, cmd, args) {
			fmt.Fprintf(os.Stderr, "hive: unknown command %q\n\n", cmd)
			printTopHelp(os.Stderr)
			os.Exit(2)
		}
	}
}

func mustDial(ctx context.Context) *ipc.Client {
	c, err := ipc.Dial(ctx, ipc.SocketPath())
	if err == nil {
		return c
	}
	if !isDaemonAbsent(err) {
		fmt.Fprintf(os.Stderr, "hive: cannot connect to hived (%v)\n", err)
		os.Exit(1)
	}
	if spawnErr := spawnDaemon(); spawnErr != nil {
		fmt.Fprintf(os.Stderr, "hive: cannot connect to hived (%v)\n", err)
		fmt.Fprintf(os.Stderr, "      auto-start failed: %v\n", spawnErr)
		os.Exit(1)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(80 * time.Millisecond)
		c, err = ipc.Dial(ctx, ipc.SocketPath())
		if err == nil {
			return c
		}
	}
	fmt.Fprintf(os.Stderr, "hive: hived was started but didn't become reachable in 3s\n")
	fmt.Fprintf(os.Stderr, "      check %s for startup errors\n", filepath.Join(ipc.StateRoot(), "hived.log"))
	os.Exit(1)
	return nil
}

// isDaemonAbsent reports whether a dial error means the daemon isn't
// running — connection-refused (socket file present, nothing listening)
// or ENOENT (no socket file at all). Other errors (permission, malformed
// path, etc.) are excluded so spawning won't paper over real config bugs.
func isDaemonAbsent(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist) {
		return true
	}
	// net.OpError wraps the syscall errno; errors.Is unwraps already, but
	// some Go versions stop at net.OpError. Belt-and-braces:
	var se syscall.Errno
	if errors.As(err, &se) {
		return se == syscall.ECONNREFUSED || se == syscall.ENOENT
	}
	return false
}

// spawnDaemon fork-exec's hived in the background, redirecting its
// output to <state_root>/hived.log. The child is detached via setsid so
// it survives this CLI process.
func spawnDaemon() error {
	hivedPath, err := locateHived()
	if err != nil {
		return err
	}
	stateRoot := ipc.StateRoot()
	if err := os.MkdirAll(stateRoot, 0o750); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}
	logPath := filepath.Join(stateRoot, "hived.log")
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	cmd := exec.Command(hivedPath)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("exec %s: %w", hivedPath, err)
	}
	// File handle stays open in the child via dup2-on-exec; close our copy.
	_ = logFile.Close()
	return nil
}

// locateHived finds the hived binary — first next to the running hive
// binary (typical install layout), then on $PATH.
func locateHived() (string, error) {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), "hived")
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, nil
		}
	}
	if p, err := exec.LookPath("hived"); err == nil {
		return p, nil
	}
	return "", errors.New("hived binary not found next to hive or on PATH")
}

func cmdVersion(ctx context.Context) {
	fmt.Printf("hive  %s\n", version.Version)
	// Try to also print the daemon version, but don't fail if daemon isn't running.
	c, err := ipc.Dial(ctx, ipc.SocketPath())
	if err != nil {
		fmt.Printf("hived (offline)\n")
		return
	}
	defer c.Close()
	raw, err := c.Call(ctx, ipc.MethodDaemonVersion, nil)
	if err != nil {
		fmt.Printf("hived (error: %v)\n", err)
		return
	}
	var r ipc.VersionResult
	_ = json.Unmarshal(raw, &r)
	fmt.Printf("hived %s\n", r.Version)
}

func cmdPing(ctx context.Context) {
	c := mustDial(ctx)
	defer c.Close()
	raw, err := c.Call(ctx, ipc.MethodDaemonPing, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ping: %v\n", err)
		os.Exit(1)
	}
	var r ipc.PingResult
	_ = json.Unmarshal(raw, &r)
	if r.OK {
		fmt.Println("pong")
	} else {
		fmt.Println("unhealthy")
		os.Exit(1)
	}
}
