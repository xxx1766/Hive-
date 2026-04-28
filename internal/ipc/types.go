package ipc

import "encoding/json"

// ── Daemon meta ───────────────────────────────────────────────────────────

type PingResult struct {
	OK bool `json:"ok"`
}

type VersionResult struct {
	Version string `json:"version"`
}

// ── Image ─────────────────────────────────────────────────────────────────

type ImageBuildParams struct {
	SourceDir string `json:"source_dir"` // directory containing agent.yaml
}

type ImageRef struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

func (r ImageRef) String() string { return r.Name + ":" + r.Version }

type ImageBuildResult struct {
	Image ImageRef `json:"image"`
	Path  string   `json:"path"` // store location
}

type ImageListResult struct {
	Images []ImageRef `json:"images"`
}

// ImagePullParams: fetch an Agent from a remote ref. URL accepts any of
// the three forms documented in internal/remote.
type ImagePullParams struct {
	URL string `json:"url"`
}

// ImagePullResult reports the pulled Image's local identity, same as
// what `image/build` returns — so the CLI can chain pull → hire using
// the returned name:version.
type ImagePullResult struct {
	Image ImageRef `json:"image"`
	Path  string   `json:"path"`
}

// ── Volume lifecycle ──────────────────────────────────────────────────────

type VolumeCreateParams struct {
	Name string `json:"name"`
}

type VolumeRef struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type VolumeListResult struct {
	Volumes []VolumeRef `json:"volumes"`
}

type VolumeRemoveParams struct {
	Name string `json:"name"`
}

// ── Room lifecycle ────────────────────────────────────────────────────────

type RoomInitParams struct {
	Name string `json:"name"` // human-friendly; daemon assigns an ID
}

type RoomInitResult struct {
	RoomID string `json:"room_id"`
	Rootfs string `json:"rootfs"`
}

type RoomRef struct {
	RoomID string `json:"room_id"`
	Name   string `json:"name"`
	State  string `json:"state"` // idle / running / stopped
}

type RoomListResult struct {
	Rooms []RoomRef `json:"rooms"`
}

type RoomStopParams struct {
	RoomID string `json:"room_id"`
}

type RoomTeamParams struct {
	RoomID string `json:"room_id"`
}

type TeamMember struct {
	ImageName string         `json:"image"`
	Rank      string         `json:"rank"`
	State     string         `json:"state"`
	Quota     map[string]any `json:"quota,omitempty"` // remaining (display-only)
}

type RoomTeamResult struct {
	RoomID  string       `json:"room_id"`
	Members []TeamMember `json:"members"`
}

// ── Agent hire ────────────────────────────────────────────────────────────

type AgentHireParams struct {
	RoomID     string            `json:"room_id"`
	Image      ImageRef          `json:"image"`
	RankName   string            `json:"rank,omitempty"`   // override manifest default
	Model      string            `json:"model,omitempty"`  // override manifest.Model (HIVE_MODEL); empty ⇒ keep manifest's
	QuotaOverr json.RawMessage   `json:"quota,omitempty"`  // override manifest quota (partial); shape = QuotaOverride
	Volumes    []VolumeMountRef  `json:"volumes,omitempty"` // bind-mount named volumes into this Agent's sandbox
}

// VolumeMountRef binds a named Volume into a hired Agent's sandbox.
//
//   Name:        volume to mount (must exist; see `hive volume create`)
//   Mode:        "ro" or "rw"
//   Mountpoint:  absolute path inside the sandbox
type VolumeMountRef struct {
	Name       string `json:"name"`
	Mode       string `json:"mode,omitempty"` // defaults to "ro"
	Mountpoint string `json:"mountpoint"`
}

// QuotaOverride mirrors the manifest's quota shape. Unmarshalled by the
// daemon from AgentHireParams.QuotaOverr and merged on top of the Rank's
// defaults — partial (key-wise) overrides are the rule, full replacement
// is expressed by setting every key.
type QuotaOverride struct {
	Tokens   map[string]int `json:"tokens,omitempty"`
	APICalls map[string]int `json:"api_calls,omitempty"`
}

type AgentHireResult struct {
	Member TeamMember `json:"member"`
}

// ── Room run (streams) ────────────────────────────────────────────────────

type RoomRunParams struct {
	RoomID string          `json:"room_id"`
	Target string          `json:"target,omitempty"` // image name of Agent to dispatch to (default: Room's "entry")
	Task   json.RawMessage `json:"task"`
}

type RoomRunResult struct {
	Output json.RawMessage `json:"output,omitempty"`
}

// ── Room logs (offline tail) ──────────────────────────────────────────────

// RoomLogsParams asks for the persisted Agent stderr logs of a Room.
// Empty Agent means "all Agents in the Room".
type RoomLogsParams struct {
	RoomID string `json:"room_id"`
	Agent  string `json:"agent,omitempty"`
}

// RoomLogEntry is one Agent's stderr snapshot.
type RoomLogEntry struct {
	Agent    string `json:"agent"`
	Path     string `json:"path"`
	Contents string `json:"contents"`
}

// RoomLogsResult bundles all requested log entries.
type RoomLogsResult struct {
	RoomID  string         `json:"room_id"`
	Entries []RoomLogEntry `json:"entries"`
}

// RoomLogNotification is the payload of NotifyRoomLog.
type RoomLogNotification struct {
	RoomID    string         `json:"room_id"`
	ImageName string         `json:"image,omitempty"`
	Level     string         `json:"level"`
	Msg       string         `json:"msg"`
	Fields    map[string]any `json:"fields,omitempty"`
	Time      string         `json:"time"`
}

// RoomStatusNotification is the payload of NotifyRoomStatus.
type RoomStatusNotification struct {
	RoomID string         `json:"room_id"`
	Event  string         `json:"event"` // agent_spawned / agent_exited / quota_exceeded / ...
	Image  string         `json:"image,omitempty"`
	Info   map[string]any `json:"info,omitempty"`
	Time   string         `json:"time"`
}

// ── Conversation lifecycle ────────────────────────────────────────────────
//
// A Conversation is a multi-round Agent collaboration with persisted
// transcript and round-cap enforcement. The lifecycle is:
//
//	create → planned → start → active → done | failed | cancelled | interrupted
//
// Cancellation reasons (carried in Error): "round_cap" (max_rounds hit),
// user-initiated, or runner-reported failure.

// ConversationCreateParams plans a new Conversation but does not dispatch
// it. The actual dispatch happens via ConversationStart, which lets a UI
// queue several conversations before kicking them off.
type ConversationCreateParams struct {
	RoomID    string          `json:"room_id"`
	Tag       string          `json:"tag,omitempty"`         // human-friendly UI label; auto if empty
	Target    string          `json:"target"`                // initial Agent (image name) the conv targets
	Input     json.RawMessage `json:"input,omitempty"`       // initial task payload
	MaxRounds int             `json:"max_rounds,omitempty"`  // 0 ⇒ DefaultMaxRounds
}

type ConversationCreateResult struct {
	ConvID string `json:"conv_id"`
	Status string `json:"status"` // "planned"
}

type ConversationStartParams struct {
	RoomID string `json:"room_id"`
	ConvID string `json:"conv_id"`
}

type ConversationStartResult struct {
	ConvID string `json:"conv_id"`
	Status string `json:"status"` // "active"
}

type ConversationListParams struct {
	RoomID string `json:"room_id"`
}

// ConversationSummary is the compact view used in list endpoints; the
// full transcript is fetched separately via Get.
type ConversationSummary struct {
	ID            string `json:"id"`
	RoomID        string `json:"room_id"`
	Tag           string `json:"tag,omitempty"`
	Status        string `json:"status"`
	InitialTarget string `json:"initial_target"`
	MaxRounds     int    `json:"max_rounds"`
	RoundCount    int    `json:"round_count"`
	MessageCount  int    `json:"message_count"`
	CreatedAt     string `json:"created_at"`
	StartedAt     string `json:"started_at,omitempty"`
	FinishedAt    string `json:"finished_at,omitempty"`
}

type ConversationListResult struct {
	RoomID        string                `json:"room_id"`
	Conversations []ConversationSummary `json:"conversations"`
}

type ConversationGetParams struct {
	RoomID string `json:"room_id"`
	ConvID string `json:"conv_id"`
}

// ConversationGetResult is the full record (json-as-bytes — the daemon
// hands the on-disk shape through unchanged so UI / CLI both deal with
// the identical schema as conversation.Conversation).
type ConversationGetResult struct {
	Conversation json.RawMessage `json:"conversation"`
}

type ConversationCancelParams struct {
	RoomID string `json:"room_id"`
	ConvID string `json:"conv_id"`
	Reason string `json:"reason,omitempty"`
}

type ConversationCancelResult struct {
	ConvID string `json:"conv_id"`
	Status string `json:"status"` // "cancelled"
}

// ConversationEventNotification is the payload of NotifyConversationEvt
// pushed during `room/run` and on the new SSE `/api/rooms/{id}/events`
// stream. Type names are stable strings (see internal/conversation/bus.go).
type ConversationEventNotification struct {
	Type    string          `json:"type"`
	RoomID  string          `json:"room_id"`
	ConvID  string          `json:"conv_id,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	Time    string          `json:"time"`
}
