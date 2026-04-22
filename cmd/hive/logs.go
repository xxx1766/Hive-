package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/anne-x/hive/internal/ipc"
)

// cmdLogs: `hive logs <room> [<agent>]` dumps Agent stderr.
//
// For a given Room, gets the content of every Agent's stderr log file
// (persisted at ~/.hive/rooms/<roomID>/logs/<agent>.stderr.log). When an
// <agent> is given, only that one; otherwise all agents, separated by
// a single header line so you can eyeball whose log is whose.
//
// No tail / follow support in this cut — user can `tail -f` the files
// directly if they need it. This command is the quickest way to see
// what went wrong after `hive run` finished.
func cmdLogs(ctx context.Context, args []string) {
	if maybeHandleHelpFlag("logs", args) {
		return
	}
	if len(args) < 1 {
		printCommandHelp("logs", os.Stderr)
		os.Exit(2)
	}
	params := ipc.RoomLogsParams{RoomID: args[0]}
	if len(args) >= 2 {
		params.Agent = args[1]
	}

	c := mustDial(ctx)
	defer c.Close()

	raw, err := c.Call(ctx, ipc.MethodRoomLogs, params)
	if err != nil {
		fmt.Fprintf(os.Stderr, "logs: %v\n", err)
		os.Exit(1)
	}
	var r ipc.RoomLogsResult
	_ = json.Unmarshal(raw, &r)

	if len(r.Entries) == 0 {
		fmt.Println("(no logs)")
		return
	}
	multiple := len(r.Entries) > 1
	for _, e := range r.Entries {
		if multiple {
			fmt.Printf("==> %s (%s) <==\n", e.Agent, e.Path)
		}
		trimmed := strings.TrimRight(e.Contents, "\n")
		if trimmed == "" {
			fmt.Println("(empty)")
		} else {
			fmt.Println(trimmed)
		}
		if multiple {
			fmt.Println()
		}
	}
}
