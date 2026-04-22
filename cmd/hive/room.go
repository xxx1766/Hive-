package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/anne-x/hive/internal/image"
	"github.com/anne-x/hive/internal/ipc"
)

func cmdInit(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("init", args) {
		return
	}
	if len(args) < 1 {
		printCommandHelp("init", os.Stderr)
		os.Exit(2)
	}
	c := mustDial(ctx)
	defer c.Close()
	raw, err := c.Call(ctx, ipc.MethodRoomInit, ipc.RoomInitParams{Name: args[0]})
	if err != nil {
		fmt.Fprintf(os.Stderr, "init: %v\n", err)
		os.Exit(1)
	}
	var r ipc.RoomInitResult
	_ = json.Unmarshal(raw, &r)
	fmt.Printf("%s\n", r.RoomID)
}

func cmdRooms(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("rooms", args) {
		return
	}
	c := mustDial(ctx)
	defer c.Close()
	raw, err := c.Call(ctx, ipc.MethodRoomList, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rooms: %v\n", err)
		os.Exit(1)
	}
	var r ipc.RoomListResult
	_ = json.Unmarshal(raw, &r)
	if len(r.Rooms) == 0 {
		fmt.Println("(no rooms)")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROOM_ID\tNAME\tSTATE")
	for _, rm := range r.Rooms {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", rm.RoomID, rm.Name, rm.State)
	}
	tw.Flush()
}

func cmdStop(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("stop", args) {
		return
	}
	if len(args) < 1 {
		printCommandHelp("stop", os.Stderr)
		os.Exit(2)
	}
	c := mustDial(ctx)
	defer c.Close()
	_, err := c.Call(ctx, ipc.MethodRoomStop, ipc.RoomStopParams{RoomID: args[0]})
	if err != nil {
		fmt.Fprintf(os.Stderr, "stop: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("stopped")
}

func cmdTeam(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("team", args) {
		return
	}
	if len(args) < 1 {
		printCommandHelp("team", os.Stderr)
		os.Exit(2)
	}
	c := mustDial(ctx)
	defer c.Close()
	raw, err := c.Call(ctx, ipc.MethodRoomTeam, ipc.RoomTeamParams{RoomID: args[0]})
	if err != nil {
		fmt.Fprintf(os.Stderr, "team: %v\n", err)
		os.Exit(1)
	}
	var r ipc.RoomTeamResult
	_ = json.Unmarshal(raw, &r)
	if len(r.Members) == 0 {
		fmt.Println("(no agents)")
		return
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "AGENT\tRANK\tSTATE\tQUOTA REMAINING")
	for _, m := range r.Members {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", m.ImageName, m.Rank, m.State, formatQuota(m.Quota))
	}
	tw.Flush()
}

func formatQuota(q map[string]any) string {
	if len(q) == 0 {
		return "(unlimited)"
	}
	parts := make([]string, 0, len(q))
	for k, v := range q {
		parts = append(parts, fmt.Sprintf("%s=%v", k, v))
	}
	return strings.Join(parts, " ")
}

func cmdHire(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("hire", args) {
		return
	}
	if len(args) < 2 {
		printCommandHelp("hire", os.Stderr)
		os.Exit(2)
	}
	roomID := args[0]
	refInput := args[1]

	rank := ""
	var quotaRaw json.RawMessage
	for i := 2; i < len(args); i++ {
		switch args[i] {
		case "--rank":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "hire: --rank requires a value")
				os.Exit(2)
			}
			rank = args[i+1]
			i++
		case "--quota":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "hire: --quota requires a JSON object")
				os.Exit(2)
			}
			if !json.Valid([]byte(args[i+1])) {
				fmt.Fprintf(os.Stderr, "hire: --quota value is not valid JSON: %s\n", args[i+1])
				os.Exit(2)
			}
			quotaRaw = json.RawMessage(args[i+1])
			i++
		}
	}

	c := mustDial(ctx)
	defer c.Close()

	// If it's a remote ref, pull into local store first and swap to
	// the resolved name:version.
	localRef, err := pullIfRemote(ctx, c, refInput)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hire: %v\n", err)
		os.Exit(1)
	}
	ref, err := image.ParseRef(localRef)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hire: %v\n", err)
		os.Exit(2)
	}

	raw, err := c.Call(ctx, ipc.MethodAgentHire, ipc.AgentHireParams{
		RoomID:     roomID,
		Image:      ipc.ImageRef{Name: ref.Name, Version: ref.Version},
		RankName:   rank,
		QuotaOverr: quotaRaw,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hire: %v\n", err)
		os.Exit(1)
	}
	var r ipc.AgentHireResult
	_ = json.Unmarshal(raw, &r)
	fmt.Printf("hired %s (rank=%s)\n", r.Member.ImageName, r.Member.Rank)
}
