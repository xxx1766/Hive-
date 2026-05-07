// Package ipc declares the CLI ↔ hived protocol: method names and payload
// shapes for `hive` subcommands. Transport is JSON-RPC 2.0 over Unix socket
// (same NDJSON framing as the Hive↔Agent channel).
package ipc

const (
	MethodDaemonPing    = "daemon/ping"
	MethodDaemonVersion = "daemon/version"

	MethodImageBuild = "image/build"
	MethodImageList  = "image/list"
	MethodImagePull  = "image/pull" // fetch a remote Agent (github://... etc.) into local store

	MethodVolumeCreate = "volume/create"
	MethodVolumeList   = "volume/list"
	MethodVolumeRemove = "volume/remove"

	MethodRoomInit   = "room/init"
	MethodRoomList   = "room/list"
	MethodRoomStop   = "room/stop"
	MethodRoomTeam   = "room/team"
	MethodRoomRun    = "room/run"    // streaming: server emits log/status notifications until final response
	MethodRoomLogs   = "room/logs"   // snapshot of Agent stderr log files
	MethodRoomRename = "room/rename" // mutate display Name; RoomID is immutable

	MethodAgentHire = "agent/hire"

	// Conversation lifecycle — multi-round Agent collaborations with
	// transcript persistence and round-cap enforcement.
	MethodConversationCreate = "conversation/create"
	MethodConversationStart  = "conversation/start"
	MethodConversationList   = "conversation/list"
	MethodConversationGet    = "conversation/get"
	MethodConversationCancel = "conversation/cancel"
)

// Notifications the daemon pushes during `room/run`.
const (
	NotifyRoomLog          = "room/log"             // structured log from an Agent
	NotifyRoomStatus       = "room/status"          // state transitions (agent spawned, quota exceeded, etc.)
	NotifyConversationEvt  = "conversation/event"   // streamed Conversation lifecycle event for UI / SSE
)
