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

const usage = `hive — Docker for Agents

Usage:
  hive <command> [args...]

Commands:
  version              print CLI + daemon version
  ping                 check the daemon is responsive
  build <dir>          package an Agent directory as a Hive Image
  images               list local Hive Images
  init <name>          create a new Room
  rooms                list Rooms
  hire <room> <image>  hire an Agent into a Room (image = name:version)
  team <room>          list Agents in a Room
  up <hivefile>        init a Room + hire all Agents declared in a Hivefile.yaml
  run <room> [task]    run a task in a Room (streams logs)
  stop <room>          stop a Room
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	switch cmd {
	case "version", "--version", "-v":
		cmdVersion(ctx)
	case "ping":
		cmdPing(ctx)
	case "help", "--help", "-h":
		fmt.Print(usage)
	default:
		// M2+ commands dispatch through dispatchCmd; keep meta commands here.
		if !dispatchCmd(ctx, cmd, args) {
			fmt.Fprintf(os.Stderr, "hive: unknown command %q\n\n%s", cmd, usage)
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
