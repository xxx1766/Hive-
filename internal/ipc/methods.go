// Package ipc declares the CLI ↔ hived protocol: method names and payload
// shapes for `hive` subcommands. Transport is JSON-RPC 2.0 over Unix socket
// (same NDJSON framing as the Hive↔Agent channel).
package ipc

const (
	MethodDaemonPing    = "daemon/ping"
	MethodDaemonVersion = "daemon/version"

	MethodImageBuild = "image/build"
	MethodImageList  = "image/list"

	MethodRoomInit = "room/init"
	MethodRoomList = "room/list"
	MethodRoomStop = "room/stop"
	MethodRoomTeam = "room/team"
	MethodRoomRun  = "room/run" // streaming: server emits log/status notifications until final response

	MethodAgentHire = "agent/hire"
)

// Notifications the daemon pushes during `room/run`.
const (
	NotifyRoomLog    = "room/log"    // structured log from an Agent
	NotifyRoomStatus = "room/status" // state transitions (agent spawned, quota exceeded, etc.)
)
