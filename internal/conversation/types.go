// Package conversation persists multi-round Agent collaborations as
// first-class entities ("Conversations"). One Conversation = a sequence
// of peer messages between two or more Agents, capped at MaxRounds.
//
// On disk, each Room holds a conversations/ subdir under
// <RoomsDir>/<roomID>/, and each Conversation lives in its own JSON file
// keyed by ConvID. The file is rewritten atomically (temp+rename) on
// every Append/Update so partial writes can never confuse readers, and a
// daemon crash mid-write leaves the previous version in place.
//
// Conversations are bound to a single Room: no cross-Room transcripts
// in v1 (would need cross-Volume routing — out of scope).
package conversation

import (
	"encoding/json"
	"time"
)

// CurrentVersion is the schema version emitted on every Save. Forward-
// compat: loaders skip files whose Version > CurrentVersion rather than
// blindly reading them.
const CurrentVersion = 1

// DefaultMaxRounds caps a Conversation that didn't specify its own.
// Calibrated for skill-style multi-Agent loops (writer ↔ outline ↔
// reviewer): 8 hops is enough for a real exchange, low enough that a
// runaway loop dies fast.
const DefaultMaxRounds = 8

// Status is the lifecycle state of a Conversation. Transitions:
//
//	planned ──start──► active ──converge──► done
//	                     │
//	                     ├── peer/send error ──► failed
//	                     ├── max_rounds hit  ──► cancelled (reason="round_cap")
//	                     └── daemon restart  ──► interrupted
type Status string

const (
	StatusPlanned     Status = "planned"
	StatusActive      Status = "active"
	StatusDone        Status = "done"
	StatusFailed      Status = "failed"
	StatusCancelled   Status = "cancelled"
	StatusInterrupted Status = "interrupted"
)

// Terminal reports whether status is end-of-life (no further transitions).
func (s Status) Terminal() bool {
	switch s {
	case StatusDone, StatusFailed, StatusCancelled, StatusInterrupted:
		return true
	}
	return false
}

// MessageKind classifies a transcript entry. The set is open — UI rendering
// keys off well-known kinds and falls back to a generic style for anything
// else, so adding a new kind never breaks older clients.
type MessageKind string

const (
	KindTaskInput  MessageKind = "task_input"  // initial input from creator
	KindPeer       MessageKind = "peer"        // agent → agent message
	KindTaskOutput MessageKind = "task_output" // agent's reply payload
	KindLog        MessageKind = "log"         // log line surfaced from a runner
	KindError      MessageKind = "error"       // tool/runner-level failure
	KindRoundCap   MessageKind = "round_cap"   // synthetic: cancellation reason
)

// Message is one entry in a Conversation transcript. Messages are
// append-only — once persisted they're never mutated.
type Message struct {
	ID      string          `json:"id"`              // monotonic per-conv: m1, m2, m3, …
	ConvID  string          `json:"conv_id"`
	From    string          `json:"from"`            // agent name; "" = system / creator
	To      string          `json:"to,omitempty"`    // agent name; "" = broadcast / no-target
	Kind    MessageKind     `json:"kind"`
	Payload json.RawMessage `json:"payload,omitempty"`
	TS      time.Time       `json:"ts"`
	Round   int             `json:"round"`           // 0 = initial input, 1+ = peer hops
}

// Conversation is the durable record. Messages are persisted inline in
// the same JSON file — the round count and message length stay bounded
// (8 default, 50 hard ceiling) so the whole file fits in a single read.
type Conversation struct {
	Version       int             `json:"version"`
	ID            string          `json:"id"`
	RoomID        string          `json:"room_id"`
	Tag           string          `json:"tag,omitempty"`           // UI-display name; default "<target>@<unix-ts>"
	Status        Status          `json:"status"`
	Participants  []string        `json:"participants,omitempty"`  // ordered as they joined
	InitialTarget string          `json:"initial_target"`          // first agent dispatched on Start
	InitialInput  json.RawMessage `json:"initial_input,omitempty"`
	MaxRounds     int             `json:"max_rounds"`
	RoundCount    int             `json:"round_count"`
	Messages      []Message       `json:"messages,omitempty"`

	CreatedAt  time.Time `json:"created_at"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	FinishedAt time.Time `json:"finished_at,omitempty"`

	FinalAnswer json.RawMessage `json:"final_answer,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// Summary is the compact projection used by list endpoints — drops the
// full transcript so a UI can render dozens of rooms without bloat.
type Summary struct {
	ID            string    `json:"id"`
	RoomID        string    `json:"room_id"`
	Tag           string    `json:"tag,omitempty"`
	Status        Status    `json:"status"`
	InitialTarget string    `json:"initial_target"`
	MaxRounds     int       `json:"max_rounds"`
	RoundCount    int       `json:"round_count"`
	MessageCount  int       `json:"message_count"`
	CreatedAt     time.Time `json:"created_at"`
	StartedAt     time.Time `json:"started_at,omitempty"`
	FinishedAt    time.Time `json:"finished_at,omitempty"`
}

// Summarize projects a Conversation to its summary form.
func (c *Conversation) Summarize() Summary {
	return Summary{
		ID:            c.ID,
		RoomID:        c.RoomID,
		Tag:           c.Tag,
		Status:        c.Status,
		InitialTarget: c.InitialTarget,
		MaxRounds:     c.MaxRounds,
		RoundCount:    c.RoundCount,
		MessageCount:  len(c.Messages),
		CreatedAt:     c.CreatedAt,
		StartedAt:     c.StartedAt,
		FinishedAt:    c.FinishedAt,
	}
}

// addParticipant adds name to Participants if not already present.
func (c *Conversation) addParticipant(name string) {
	if name == "" {
		return
	}
	for _, p := range c.Participants {
		if p == name {
			return
		}
	}
	c.Participants = append(c.Participants, name)
}
