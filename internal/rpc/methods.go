// Package rpc declares method names and parameter shapes for the
// Hive daemon ↔ Agent JSON-RPC channel. This is the API seen by Agent authors.
package rpc

// Hive → Agent methods.
const (
	MethodTaskRun    = "task/run"    // dispatch a task to the Agent
	MethodPeerRecv   = "peer/recv"   // inbound message from another Agent in the Room
	MethodEventsRecv = "events/recv" // inbound event from a publisher (same Room or cross-Room via Volume)
	MethodShutdown   = "shutdown"    // graceful termination signal
)

// Agent → Hive methods.
const (
	MethodFsRead      = "fs/read"
	MethodFsWrite     = "fs/write"
	MethodFsList      = "fs/list"
	MethodNetFetch    = "net/fetch"
	MethodLLMComplete = "llm/complete"
	MethodPeerSend    = "peer/send"
	MethodMemoryPut    = "memory/put"
	MethodMemoryGet    = "memory/get"
	MethodMemoryList   = "memory/list"
	MethodMemoryDelete = "memory/delete"
	MethodEventsPublish     = "events/publish"
	MethodEventsSubscribe   = "events/subscribe"
	MethodEventsUnsubscribe = "events/unsubscribe"
	MethodAIToolInvoke = "ai_tool/invoke"
	// MethodHireJunior lets a manager+ rank Agent spawn a subordinate at
	// runtime. The daemon validates the caller's rank against the
	// requested rank (rank.CanHire) and atomically carves the requested
	// quota out of the caller's remaining budget so subordinates can
	// never escalate the parent's effective allotment.
	MethodHireJunior  = "hire/junior"
	MethodTaskDone    = "task/done"
	MethodTaskError   = "task/error"
	MethodLog         = "log"
)
