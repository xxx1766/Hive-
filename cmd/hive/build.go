package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/anne-x/hive/internal/ipc"
)

func cmdBuild(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("build", args) {
		return
	}
	if len(args) < 1 {
		printCommandHelp("build", os.Stderr)
		os.Exit(2)
	}
	c := mustDial(ctx)
	defer c.Close()
	raw, err := c.Call(ctx, ipc.MethodImageBuild, ipc.ImageBuildParams{SourceDir: args[0]})
	if err != nil {
		fmt.Fprintf(os.Stderr, "build: %v\n", err)
		os.Exit(1)
	}
	var r ipc.ImageBuildResult
	_ = json.Unmarshal(raw, &r)
	fmt.Printf("built %s at %s\n", r.Image, r.Path)
}

// cmdAgents lists the locally-installed Agents (Hive Images in the store).
// The CLI surface name is `agents` because that's what the user thinks of
// — "Hive Image" stays as the internal packaging concept.
func cmdAgents(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("agents", args) {
		return
	}
	c := mustDial(ctx)
	defer c.Close()
	raw, err := c.Call(ctx, ipc.MethodImageList, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "agents: %v\n", err)
		os.Exit(1)
	}
	var r ipc.ImageListResult
	_ = json.Unmarshal(raw, &r)
	if len(r.Images) == 0 {
		fmt.Println("(no agents)")
		return
	}
	for _, img := range r.Images {
		fmt.Printf("  %s\n", img)
	}
}
