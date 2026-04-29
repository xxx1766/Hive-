// Package roomstate persists each Room's hire manifest so the daemon can
// recover Rooms after a restart. State lives at
// <RoomsDir>/<roomID>/state.json — same per-Room directory the rootfs/logs
// already use, so a single os.RemoveAll wipes everything when a Room is
// explicitly stopped.
//
// What's persisted is the *minimum needed to re-run agent/hire*: image
// ref, rank override, quota override (wire form), volume mounts (wire
// form). Conn, router, ctx, sub_ids, quota counters and any kernel-side
// namespace state are deliberately *not* persisted — they belong to the
// process generation and get rebuilt on recovery.
//
// Concurrency: Save and Delete take a per-RoomID mutex so two goroutines
// (e.g. concurrent OnAgentExit reapers) can't write half-stale snapshots
// over each other. The atomic temp+rename inside Save guarantees readers
// never see a partially-written file even across daemon crashes.
package roomstate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anne-x/hive/internal/ipc"
)

// CurrentVersion is the schema version emitted by Save. LoadAll skips any
// snapshot whose Version exceeds this (forward-compat safety: an older
// daemon shouldn't blindly act on a newer schema).
const CurrentVersion = 1

const stateFile = "state.json"

// Snapshot is the on-disk shape of one Room's recoverable state.
type Snapshot struct {
	Version int          `json:"version"`
	Name    string       `json:"name"`
	Members []MemberSnap `json:"members"`
	SavedAt time.Time    `json:"saved_at"`
}

// MemberSnap mirrors ipc.AgentHireParams (sans RoomID) so recovery can
// feed it back into the same hire code path the IPC handler uses.
type MemberSnap struct {
	Image     ipc.ImageRef         `json:"image"`
	RankName  string               `json:"rank,omitempty"`
	Model     string               `json:"model,omitempty"`
	QuotaOver json.RawMessage      `json:"quota,omitempty"`
	Volumes   []ipc.VolumeMountRef `json:"volumes,omitempty"`
	HiredAt   time.Time            `json:"hired_at"`
	// Parent (when set) records the in-room name of the auto-hiring Agent.
	// Empty for top-level hires (CLI / Hivefile). Recovery preserves this
	// so the subordinate-tree shape survives daemon restart.
	Parent    string               `json:"parent,omitempty"`
	// Name (when set) overrides the default in-room identity (which
	// defaults to Image.Name). Allows the same image to be hired
	// multiple times with distinct aliases. Omitted from JSON when
	// equal to Image.Name to keep state.json terse for the common case.
	Name      string               `json:"name,omitempty"`
}

// Loaded is one Snapshot plus its derived RoomID (the directory name).
// Keeping RoomID out of the on-disk payload avoids drift if a directory
// is moved/renamed manually.
type Loaded struct {
	RoomID string
	Snapshot
}

// per-RoomID locks; map-of-mutexes pattern. A bigger codebase might use
// sync.Map, but a plain map+mutex is simpler and the daemon's RoomID set
// is bounded.
var (
	locksMu sync.Mutex
	locks   = map[string]*sync.Mutex{}
)

func lockFor(roomID string) *sync.Mutex {
	locksMu.Lock()
	defer locksMu.Unlock()
	l, ok := locks[roomID]
	if !ok {
		l = &sync.Mutex{}
		locks[roomID] = l
	}
	return l
}

// Save writes snap to <roomsDir>/<roomID>/state.json atomically. The
// per-RoomID mutex serialises concurrent writers; temp+rename means a
// crash mid-write leaves the previous version in place.
func Save(roomsDir, roomID string, snap *Snapshot) error {
	if snap == nil {
		return errors.New("roomstate: nil snapshot")
	}
	if snap.Version == 0 {
		snap.Version = CurrentVersion
	}
	snap.SavedAt = time.Now().UTC()

	l := lockFor(roomID)
	l.Lock()
	defer l.Unlock()

	dir := filepath.Join(roomsDir, roomID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("roomstate: mkdir: %w", err)
	}

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("roomstate: marshal: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return fmt.Errorf("roomstate: tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("roomstate: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("roomstate: close: %w", err)
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, stateFile)); err != nil {
		cleanup()
		return fmt.Errorf("roomstate: rename: %w", err)
	}
	return nil
}

// LoadAll walks roomsDir and returns one Loaded per recoverable Room.
// Missing directory ⇒ empty result with nil error (first daemon boot).
// Per-Room failures (corrupt JSON, future-version, missing state.json,
// permission errors) are logged-skip: they never block other Rooms.
//
// Returned slice is unsorted; callers that care about determinism should
// sort by RoomID.
func LoadAll(roomsDir string) ([]Loaded, error) {
	entries, err := os.ReadDir(roomsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Loaded
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		roomID := e.Name()
		path := filepath.Join(roomsDir, roomID, stateFile)
		data, err := os.ReadFile(path)
		if err != nil {
			// Includes ENOENT (room dir without state.json — leftover from a
			// pre-persistence Hive or a manually rm'd state file). Skip.
			continue
		}
		var snap Snapshot
		if err := json.Unmarshal(data, &snap); err != nil {
			continue
		}
		if snap.Version > CurrentVersion {
			// A future daemon wrote this. Don't pretend we understand it.
			continue
		}
		out = append(out, Loaded{RoomID: roomID, Snapshot: snap})
	}
	return out, nil
}

// Delete removes the Room's state file. Idempotent — ENOENT is success,
// since the post-condition (no state file) is already met. Any other
// error is returned so the caller can surface persistence trouble.
func Delete(roomsDir, roomID string) error {
	l := lockFor(roomID)
	l.Lock()
	defer l.Unlock()

	path := filepath.Join(roomsDir, roomID, stateFile)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
