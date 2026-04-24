package rpc

import "encoding/json"

// ── Hive → Agent ──────────────────────────────────────────────────────────

// TaskRunParams is delivered when Hive wants the Agent to start working.
// Input is an arbitrary JSON payload the Agent understands.
type TaskRunParams struct {
	TaskID string          `json:"task_id"`
	Goal   string          `json:"goal,omitempty"`
	Input  json.RawMessage `json:"input,omitempty"`
}

// PeerRecvParams carries an inbound message from another Agent in the same Room.
type PeerRecvParams struct {
	From    string          `json:"from"`    // source Agent's image name
	Payload json.RawMessage `json:"payload"` // opaque to Hive
}

// ShutdownParams is empty — presence of the method is the signal.
type ShutdownParams struct {
	Reason string `json:"reason,omitempty"`
}

// ── Agent → Hive: filesystem (routed through fsproxy) ─────────────────────

type FsReadParams struct {
	Path string `json:"path"`
}

type FsReadResult struct {
	Data []byte `json:"data"` // base64 via json.Marshal
}

type FsWriteParams struct {
	Path string `json:"path"`
	Data []byte `json:"data"`
}

type FsListParams struct {
	Path string `json:"path"`
}

type FsEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

type FsListResult struct {
	Entries []FsEntry `json:"entries"`
}

// ── Agent → Hive: network ─────────────────────────────────────────────────

type NetFetchParams struct {
	Method  string            `json:"method"`            // GET / POST / ...
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

type NetFetchResult struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// ── Agent → Hive: LLM ─────────────────────────────────────────────────────

type LLMMessage struct {
	Role    string `json:"role"`    // system / user / assistant
	Content string `json:"content"`
}

type LLMCompleteParams struct {
	Provider  string       `json:"provider,omitempty"` // openai / anthropic / mock (default from daemon config)
	Model     string       `json:"model"`
	Messages  []LLMMessage `json:"messages"`
	MaxTokens int          `json:"max_tokens,omitempty"`
}

type LLMUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type LLMCompleteResult struct {
	Text  string   `json:"text"`
	Usage LLMUsage `json:"usage"`
}

// ── Agent → Hive: memory (persistent KV, Room-private or Volume-shared) ──

// Scope:
//   ""           → private to the calling Room (survives daemon restarts)
//   "<volname>"  → shared, stored under the named Volume

type MemoryPutParams struct {
	Scope string `json:"scope,omitempty"`
	Key   string `json:"key"`
	Value []byte `json:"value"`
}

type MemoryGetParams struct {
	Scope string `json:"scope,omitempty"`
	Key   string `json:"key"`
}

// Exists=false + Value=nil means "no such key" (not an error).
type MemoryGetResult struct {
	Value  []byte `json:"value,omitempty"`
	Exists bool   `json:"exists"`
}

type MemoryListParams struct {
	Scope  string `json:"scope,omitempty"`
	Prefix string `json:"prefix,omitempty"`
}

type MemoryListResult struct {
	Keys []string `json:"keys"`
}

type MemoryDeleteParams struct {
	Scope string `json:"scope,omitempty"`
	Key   string `json:"key"`
}

// ── Agent → Hive: ai_tool (CLI AI tools as computational backend) ────────

// AIToolInvokeParams dispatches a prompt to a registered ai-tool Provider
// (MVP: "claude-code"). The tool runs with cwd = the calling Room's
// workspace dir on the host; any file output lands in /workspace inside
// the Agent's sandbox.
type AIToolInvokeParams struct {
	Tool    string `json:"tool"`              // "claude-code" in MVP
	Prompt  string `json:"prompt"`
	Timeout int    `json:"timeout,omitempty"` // seconds; 0 ⇒ daemon default (300)
}

type AIToolInvokeResult struct {
	Output   string `json:"output"`
	Stderr   string `json:"stderr,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// ── Agent → Hive: peer messaging ──────────────────────────────────────────

type PeerSendParams struct {
	To      string          `json:"to"` // target Agent's image name (unique within Room)
	Payload json.RawMessage `json:"payload"`
}

// ── Agent → Hive: task termination ────────────────────────────────────────

type TaskDoneParams struct {
	TaskID string          `json:"task_id"`
	Output json.RawMessage `json:"output,omitempty"`
}

type TaskErrorParams struct {
	TaskID  string `json:"task_id"`
	Code    int    `json:"code,omitempty"`
	Message string `json:"message"`
}

// ── Agent → Hive: structured log ──────────────────────────────────────────

type LogParams struct {
	Level  string         `json:"level"` // debug / info / warn / error
	Msg    string         `json:"msg"`
	Fields map[string]any `json:"fields,omitempty"`
}
