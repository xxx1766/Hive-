package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/anne-x/hive/internal/ipc"
)

// cmdVolume is the `hive volume <subcmd>` dispatcher. Subcommands:
//   create <name>  — new named volume at ~/.hive/volumes/<name>/
//   ls             — list all volumes
//   rm <name>      — delete (idempotent)
func cmdVolume(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("volume", args) {
		return
	}
	if len(args) < 1 {
		printCommandHelp("volume", os.Stderr)
		os.Exit(2)
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "create":
		cmdVolumeCreate(ctx, rest)
	case "ls", "list":
		cmdVolumeList(ctx, rest)
	case "rm", "remove":
		cmdVolumeRemove(ctx, rest)
	default:
		fmt.Fprintf(os.Stderr, "hive volume: unknown subcommand %q\n\n", sub)
		printCommandHelp("volume", os.Stderr)
		os.Exit(2)
	}
}

func cmdVolumeCreate(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: hive volume create <name>")
		os.Exit(2)
	}
	c := mustDial(ctx)
	defer c.Close()
	raw, err := c.Call(ctx, ipc.MethodVolumeCreate, ipc.VolumeCreateParams{Name: args[0]})
	if err != nil {
		fmt.Fprintf(os.Stderr, "volume create: %v\n", err)
		os.Exit(1)
	}
	var v ipc.VolumeRef
	_ = json.Unmarshal(raw, &v)
	fmt.Printf("created %s at %s\n", v.Name, v.Path)
}

func cmdVolumeList(ctx context.Context, args []string) {
	c := mustDial(ctx)
	defer c.Close()
	raw, err := c.Call(ctx, ipc.MethodVolumeList, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "volume ls: %v\n", err)
		os.Exit(1)
	}
	var r ipc.VolumeListResult
	_ = json.Unmarshal(raw, &r)
	if len(r.Volumes) == 0 {
		fmt.Println("(no volumes)")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPATH")
	for _, v := range r.Volumes {
		fmt.Fprintf(tw, "%s\t%s\n", v.Name, v.Path)
	}
	tw.Flush()
}

func cmdVolumeRemove(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: hive volume rm <name>")
		os.Exit(2)
	}
	c := mustDial(ctx)
	defer c.Close()
	_, err := c.Call(ctx, ipc.MethodVolumeRemove, ipc.VolumeRemoveParams{Name: args[0]})
	if err != nil {
		fmt.Fprintf(os.Stderr, "volume rm: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("removed")
}
