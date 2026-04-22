package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/anne-x/hive/internal/hivefile"
	"github.com/anne-x/hive/internal/image"
	"github.com/anne-x/hive/internal/ipc"
)

// cmdUp: `hive up <hivefile>` — init a Room and hire all declared Agents.
// Prints the Room ID on success so callers can pipe it into `hive run`.
func cmdUp(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: hive up <hivefile.yaml>")
		os.Exit(2)
	}
	hfPath := args[0]
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

	// 2. Hire each declared Agent.
	for _, a := range hf.Agents {
		ref, _ := image.ParseRef(a.Image) // pre-validated by hivefile.Load
		_, err := c.Call(ctx, ipc.MethodAgentHire, ipc.AgentHireParams{
			RoomID:   init.RoomID,
			Image:    ipc.ImageRef{Name: ref.Name, Version: ref.Version},
			RankName: a.Rank,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "up/hire %s: %v\n", a.Image, err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "  hired %s\n", a.Image)
	}

	// 3. Print RoomID to stdout so shell scripts can capture it.
	fmt.Println(init.RoomID)
}
