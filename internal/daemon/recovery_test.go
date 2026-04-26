package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/anne-x/hive/internal/ipc"
	"github.com/anne-x/hive/internal/roomstate"
)

// echoBinaryAvailable returns true if the prebuilt examples/echo binary
// (a tiny stdio agent used as the test fixture) is present. CI runs
// `make build` which produces it; bare clones may not. Tests gate
// themselves on this so a fresh checkout doesn't fail.
func echoBinaryAvailable() (string, bool) {
	if runtime.GOOS != "linux" {
		return "", false
	}
	// The repo root is two levels above this test file's package.
	_, thisFile, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	src := filepath.Join(root, "examples", "echo")
	bin := filepath.Join(src, "bin", "echo")
	if _, err := os.Stat(bin); err != nil {
		return "", false
	}
	return src, true
}

// withDaemon spins up a Daemon rooted at a tempdir state, returns the
// Daemon and a cleanup that calls Shutdown. HIVE_NO_SANDBOX bypasses
// CLONE_NEWNS so this works as non-root.
func withDaemon(t *testing.T) *Daemon {
	t.Helper()
	state := t.TempDir()
	t.Setenv("HIVE_STATE", state)
	t.Setenv("HIVE_NO_SANDBOX", "1")
	d, err := New()
	if err != nil {
		t.Fatalf("daemon.New: %v", err)
	}
	return d
}

func TestRecoverEmptyRoom(t *testing.T) {
	d := withDaemon(t)
	ctx := context.Background()

	// Create a Room with no agents.
	initParams, _ := json.Marshal(ipc.RoomInitParams{Name: "lonely"})
	res, err := d.handleRoomInit(ctx, initParams, nil)
	if err != nil {
		t.Fatalf("handleRoomInit: %v", err)
	}
	roomID := res.(ipc.RoomInitResult).RoomID

	// state.json should exist immediately — even with no members.
	statePath := filepath.Join(ipc.RoomsDir(), roomID, "state.json")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state.json missing after handleRoomInit: %v", err)
	}

	d.Shutdown()

	// Restart with the same state-dir. The Room should come back.
	d2, err := New()
	if err != nil {
		t.Fatalf("daemon.New (restart): %v", err)
	}
	defer d2.Shutdown()

	listRes, err := d2.handleRoomList(ctx, nil, nil)
	if err != nil {
		t.Fatalf("handleRoomList: %v", err)
	}
	rooms := listRes.(ipc.RoomListResult).Rooms
	if len(rooms) != 1 || rooms[0].RoomID != roomID || rooms[0].Name != "lonely" {
		t.Fatalf("recovered rooms = %+v, want one room %q named lonely", rooms, roomID)
	}
}

func TestRoomStopDeletesState(t *testing.T) {
	d := withDaemon(t)
	ctx := context.Background()

	initParams, _ := json.Marshal(ipc.RoomInitParams{Name: "ephemeral"})
	res, err := d.handleRoomInit(ctx, initParams, nil)
	if err != nil {
		t.Fatalf("handleRoomInit: %v", err)
	}
	roomID := res.(ipc.RoomInitResult).RoomID
	statePath := filepath.Join(ipc.RoomsDir(), roomID, "state.json")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("state.json missing: %v", err)
	}

	stopParams, _ := json.Marshal(ipc.RoomStopParams{RoomID: roomID})
	if _, err := d.handleRoomStop(ctx, stopParams, nil); err != nil {
		t.Fatalf("handleRoomStop: %v", err)
	}
	if _, err := os.Stat(statePath); !os.IsNotExist(err) {
		t.Fatalf("state.json should be gone after room/stop, stat err = %v", err)
	}

	d.Shutdown()
	d2, err := New()
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	defer d2.Shutdown()
	listRes, _ := d2.handleRoomList(ctx, nil, nil)
	rooms := listRes.(ipc.RoomListResult).Rooms
	if len(rooms) != 0 {
		t.Fatalf("stopped room should not recover, got %+v", rooms)
	}
}

func TestRecoverRoomWithAgent(t *testing.T) {
	echoSrc, ok := echoBinaryAvailable()
	if !ok {
		t.Skip("examples/echo binary not built; run `make build` first")
	}
	d := withDaemon(t)
	ctx := context.Background()

	// Build the echo image into the daemon's local store. Mirrors what
	// `hive image build examples/echo` does at the CLI.
	img, err := d.store.Put(echoSrc)
	if err != nil {
		t.Fatalf("store.Put echo: %v", err)
	}

	initParams, _ := json.Marshal(ipc.RoomInitParams{Name: "withecho"})
	res, err := d.handleRoomInit(ctx, initParams, nil)
	if err != nil {
		t.Fatalf("handleRoomInit: %v", err)
	}
	roomID := res.(ipc.RoomInitResult).RoomID

	hireParams, _ := json.Marshal(ipc.AgentHireParams{
		RoomID: roomID,
		Image:  ipc.ImageRef{Name: img.Manifest.Name, Version: img.Manifest.Version},
	})
	if _, err := d.handleAgentHire(ctx, hireParams, nil); err != nil {
		t.Fatalf("handleAgentHire: %v", err)
	}

	// state.json should now list the echo member.
	loaded, err := roomstate.LoadAll(ipc.RoomsDir())
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(loaded) != 1 || len(loaded[0].Members) != 1 ||
		loaded[0].Members[0].Image.Name != "echo" {
		t.Fatalf("persisted snapshot wrong: %+v", loaded)
	}

	d.Shutdown()

	// Restart with the same state-dir. echo should be re-hired automatically.
	d2, err := New()
	if err != nil {
		t.Fatalf("New (restart): %v", err)
	}
	defer d2.Shutdown()

	// recoverRooms is synchronous in New() so by the time we get here,
	// the agent should already be hired (or recovery should have logged
	// a failure). Confirm immediately.
	teamParams, _ := json.Marshal(ipc.RoomTeamParams{RoomID: roomID})
	teamRes, err := d2.handleRoomTeam(ctx, teamParams, nil)
	if err != nil {
		t.Fatalf("handleRoomTeam: %v", err)
	}
	team := teamRes.(ipc.RoomTeamResult)
	if len(team.Members) != 1 || team.Members[0].ImageName != "echo" {
		// Dump on-disk and in-memory state to make the failure debuggable.
		snaps, _ := roomstate.LoadAll(ipc.RoomsDir())
		t.Fatalf("recovery failed: team=%+v, persisted snaps=%+v", team, snaps)
	}
	// End-to-end: dispatch a task to the recovered echo. If the agent
	// process and proxies were re-wired correctly, room/run will round-trip.
	taskInput := json.RawMessage(`"hello-after-restart"`)
	runParams, _ := json.Marshal(ipc.RoomRunParams{
		RoomID: roomID,
		Target: "echo",
		Task:   taskInput,
	})
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	runRes, err := d2.handleRoomRun(runCtx, runParams, func(method string, params any) {})
	if err != nil {
		t.Fatalf("handleRoomRun on recovered agent: %v", err)
	}
	out := runRes.(ipc.RoomRunResult).Output
	if string(out) != string(taskInput) {
		t.Fatalf("echo output drift: got %s want %s", out, taskInput)
	}
}

// TestAgentVoluntaryExitUpdatesSnapshot verifies the *other* side of
// the OnAgentExit branch: when an Agent exits while the daemon is
// running normally (not shutting down), the snapshot must be rewritten
// so a future recovery doesn't try to re-hire a dead Agent.
func TestAgentVoluntaryExitUpdatesSnapshot(t *testing.T) {
	echoSrc, ok := echoBinaryAvailable()
	if !ok {
		t.Skip("examples/echo binary not built")
	}
	d := withDaemon(t)
	defer d.Shutdown()
	ctx := context.Background()

	img, err := d.store.Put(echoSrc)
	if err != nil {
		t.Fatalf("store.Put: %v", err)
	}
	initParams, _ := json.Marshal(ipc.RoomInitParams{Name: "exittest"})
	res, _ := d.handleRoomInit(ctx, initParams, nil)
	roomID := res.(ipc.RoomInitResult).RoomID

	hireParams, _ := json.Marshal(ipc.AgentHireParams{
		RoomID: roomID,
		Image:  ipc.ImageRef{Name: img.Manifest.Name, Version: img.Manifest.Version},
	})
	if _, err := d.handleAgentHire(ctx, hireParams, nil); err != nil {
		t.Fatalf("hire: %v", err)
	}

	loaded, _ := roomstate.LoadAll(ipc.RoomsDir())
	if len(loaded) != 1 || len(loaded[0].Members) != 1 {
		t.Fatalf("pre-exit snapshot wrong: %+v", loaded)
	}

	// Kill just this Agent's connection — daemon stays up.
	d.mu.RLock()
	r := d.rooms[roomID]
	d.mu.RUnlock()
	m := r.Member("echo")
	if m == nil {
		t.Fatal("echo member missing")
	}
	_ = m.Conn.Kill()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		loaded, _ := roomstate.LoadAll(ipc.RoomsDir())
		if len(loaded) == 1 && len(loaded[0].Members) == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	loaded, _ = roomstate.LoadAll(ipc.RoomsDir())
	t.Fatalf("snapshot still lists exited agent: %+v", loaded)
}
