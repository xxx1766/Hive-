package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	if err != nil {
		fmt.Fprintf(os.Stderr, "hive: cannot connect to hived (%v)\n", err)
		fmt.Fprintf(os.Stderr, "      is the daemon running? try: hived &\n")
		os.Exit(1)
	}
	return c
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
