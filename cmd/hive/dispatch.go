package main

import (
	"context"
)

// dispatchCmd returns true if cmd was handled.
func dispatchCmd(ctx context.Context, cmd string, args []string) bool {
	switch cmd {
	case "build":
		cmdBuild(ctx, args)
	case "images":
		cmdImages(ctx, args)
	case "init":
		cmdInit(ctx, args)
	case "rooms":
		cmdRooms(ctx, args)
	case "stop":
		cmdStop(ctx, args)
	case "team":
		cmdTeam(ctx, args)
	case "hire":
		cmdHire(ctx, args)
	case "run":
		cmdRun(ctx, args)
	case "up":
		cmdUp(ctx, args)
	case "pull":
		cmdPull(ctx, args)
	case "logs":
		cmdLogs(ctx, args)
	case "volume":
		cmdVolume(ctx, args)
	default:
		return false
	}
	return true
}
