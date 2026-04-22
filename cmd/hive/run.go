package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/anne-x/hive/internal/ipc"
)

func cmdRun(ctx context.Context, args []string) {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: hive run <room> [--target <image>] [task-json]")
		os.Exit(2)
	}
	roomID := args[0]
	target := ""
	var taskJSON json.RawMessage
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--target" && i+1 < len(args):
			target = args[i+1]
			i++
		default:
			// Anything else is treated as the task JSON string.
			taskJSON = json.RawMessage(a)
		}
	}
	if taskJSON == nil {
		taskJSON = json.RawMessage("null")
	} else if !isValidJSON(taskJSON) {
		// Wrap plain strings in JSON quotes so agents that expect a string get one.
		taskJSON, _ = json.Marshal(string(taskJSON))
	}

	c := mustDial(ctx)
	defer c.Close()

	c.SetNotifyHandler(func(method string, params json.RawMessage) {
		switch method {
		case ipc.NotifyRoomLog:
			var n ipc.RoomLogNotification
			if err := json.Unmarshal(params, &n); err == nil {
				printLog(n)
			}
		case ipc.NotifyRoomStatus:
			var n ipc.RoomStatusNotification
			if err := json.Unmarshal(params, &n); err == nil {
				printStatus(n)
			}
		}
	})

	raw, err := c.Call(ctx, ipc.MethodRoomRun, ipc.RoomRunParams{
		RoomID: roomID,
		Target: target,
		Task:   taskJSON,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
		os.Exit(1)
	}
	var r ipc.RoomRunResult
	_ = json.Unmarshal(raw, &r)
	fmt.Println("──")
	if len(r.Output) > 0 {
		fmt.Printf("output: %s\n", string(r.Output))
	} else {
		fmt.Println("done (no output)")
	}
}

func isValidJSON(b []byte) bool {
	var v any
	return json.Unmarshal(b, &v) == nil
}

func printLog(n ipc.RoomLogNotification) {
	level := strings.ToUpper(n.Level)
	if level == "" {
		level = "INFO"
	}
	fmt.Printf("[%s] %s: %s\n", level, n.ImageName, n.Msg)
}

func printStatus(n ipc.RoomStatusNotification) {
	if n.Image != "" {
		fmt.Printf("[STATUS] %s %s\n", n.Event, n.Image)
	} else {
		fmt.Printf("[STATUS] %s\n", n.Event)
	}
}
