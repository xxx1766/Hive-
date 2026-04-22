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
	RoomID     string          `json:"room_id"`
	Image      ImageRef        `json:"image"`
	RankName   string          `json:"rank,omitempty"`  // override manifest default
	QuotaOverr json.RawMessage `json:"quota,omitempty"` // override manifest quota (partial)
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
