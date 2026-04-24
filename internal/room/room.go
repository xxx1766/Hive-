// Package room owns one Room's state: its rootfs, its Agents, its Router.
//
// Namespace isolation happens here (see Spawn). For M2 we accept that
// Cloneflags may fall back to "no namespace" when HIVE_NO_SANDBOX=1 is set,
// which lets the test suite run in non-root CI environments. Real demo
// requires root so we can CLONE_NEWNS / CLONE_NEWNET.
package room

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anne-x/hive/internal/agent"
	"github.com/anne-x/hive/internal/image"
	"github.com/anne-x/hive/internal/ipc"
	"github.com/anne-x/hive/internal/ns"
	"github.com/anne-x/hive/internal/rank"
	"github.com/anne-x/hive/internal/rpc"
	"github.com/anne-x/hive/internal/router"
)

// State reflects the Room's lifecycle phase.
type State string

const (
	StateIdle    State = "idle"
	StateRunning State = "running"
	StateStopped State = "stopped"
)

// Member tracks a hired Agent within a Room.
type Member struct {
	Image image.Ref
	Rank  *rank.Rank
	// QuotaOverride is set when Hivefile / CLI overrides the Rank's default
	// quota for this specific hire. nil ⇒ use Rank.Quota unchanged.
	QuotaOverride *rank.Quota
	// Mounts records the volumes bind-mounted into this Agent's sandbox.
	// The daemon uses it to construct the Agent's effective FS allow-list.
	Mounts  []ns.Mount
	Conn    *agent.Conn
	HiredAt time.Time
}

// EffectiveQuota merges the Rank's default quota with any per-hire override.
// Override keys replace base values; base keys absent from the override are
// preserved. Both buckets (Tokens, APICalls) are merged independently.
func (m *Member) EffectiveQuota() rank.Quota {
	base := m.Rank.Quota
	if m.QuotaOverride == nil {
		return base
	}
	out := rank.Quota{
		Tokens:   make(map[string]int, len(base.Tokens)),
		APICalls: make(map[string]int, len(base.APICalls)),
	}
	for k, v := range base.Tokens {
		out.Tokens[k] = v
	}
	for k, v := range base.APICalls {
		out.APICalls[k] = v
	}
	for k, v := range m.QuotaOverride.Tokens {
		out.Tokens[k] = v
	}
	for k, v := range m.QuotaOverride.APICalls {
		out.APICalls[k] = v
	}
	return out
}

// Room is the unit of isolation. One Go routine owns the Router; all
// Agents are children of the daemon process and addressable by image name.
type Room struct {
	ID     string
	Name   string
	Rootfs string
	State  State

	mu      sync.RWMutex
	members map[string]*Member // key: image name (unique in Room)
	router  *router.Router

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// Hooks are set by the daemon when the Room is created. Keeping them
	// as function fields (instead of an interface) lets the room package
	// avoid importing daemon/proxy packages — breaks an import cycle.
	Hooks Hooks
}

// Hooks are injection points for cross-cutting services that live in the
// daemon but need per-Room scope. Each Room stores its own, initialised
// by the caller at New().
type Hooks struct {
	// OnLog is called whenever an Agent emits a log message.
	OnLog func(imageName string, params rpc.LogParams)
	// OnStatus reports lifecycle events (spawned, exited, quota exceeded).
	OnStatus func(event, imageName string, info map[string]any)
	// RegisterAgentHandlers installs Agent→Hive handlers on a Conn at
	// spawn time (fs/*, net/fetch, llm/complete, quota accounting).
	RegisterAgentHandlers func(r *Room, m *Member)
	// AuthPeerSend is installed on the Router; returns non-nil *protocol.Error
	// to reject a peer/send.
	AuthPeerSend func(r *Room, from, to string) error
}

// New creates an idle Room with its rootfs directory.
func New(id, name string, hooks Hooks) (*Room, error) {
	rootfs := filepath.Join(ipc.RoomsDir(), id, "rootfs")
	if err := os.MkdirAll(rootfs, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir rootfs: %w", err)
	}
	logsDir := filepath.Join(ipc.RoomsDir(), id, "logs")
	if err := os.MkdirAll(logsDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir logs: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &Room{
		ID:      id,
		Name:    name,
		Rootfs:  rootfs,
		State:   StateIdle,
		members: make(map[string]*Member),
		ctx:     ctx,
		cancel:  cancel,
		done:    make(chan struct{}),
		Hooks:   hooks,
	}
	// Build the router. The AuthFn closes over r so Room-scoped Rank
	// checks happen in one place.
	r.router = router.New(id, func(from, to string) error {
		if r.Hooks.AuthPeerSend != nil {
			return r.Hooks.AuthPeerSend(r, from, to)
		}
		return nil
	}, 0)
	go r.router.Run(ctx)
	return r, nil
}

// Ref is the public info tuple used by `hive rooms`.
func (r *Room) Ref() ipc.RoomRef {
	return ipc.RoomRef{RoomID: r.ID, Name: r.Name, State: string(r.State)}
}

// Members returns a snapshot of current members (read-only slice).
func (r *Room) Members() []*Member {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Member, 0, len(r.members))
	for _, m := range r.members {
		out = append(out, m)
	}
	return out
}

// Member returns a member by image name, or nil if not present.
func (r *Room) Member(imageName string) *Member {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.members[imageName]
}

// HireOpts carries all the per-hire knobs that grew past what fits in
// positional args. Future knobs (attach policy, resource class, ...) go here.
type HireOpts struct {
	Rank          *rank.Rank
	QuotaOverride *rank.Quota // nil means "use Rank.Quota defaults"
	Mounts        []ns.Mount  // extra bind mounts inside the sandbox (volumes)
	LogFile       *os.File
	ExtraEnv      []string
}

// Hire spawns an Agent process and attaches it to this Room.
// extraEnv (via opts.ExtraEnv) is appended to the child process's
// environment — the daemon uses this to pass kind-specific knobs
// (HIVE_SKILL_PATH, HIVE_WORKFLOW_PATH, …) without the room package
// having to know about each Agent kind.
func (r *Room) Hire(img *image.Image, opts HireOpts) (*Member, error) {
	rk := opts.Rank
	logFile := opts.LogFile
	extraEnv := opts.ExtraEnv
	r.mu.Lock()
	if _, dup := r.members[img.Manifest.Name]; dup {
		r.mu.Unlock()
		return nil, fmt.Errorf("agent %s already hired in room %s", img.Manifest.Name, r.ID)
	}
	r.mu.Unlock()

	cmd, err := ns.NewAgentCommand(r.Rootfs, img.Dir, img.Manifest.Entry, opts.Mounts)
	if err != nil {
		return nil, fmt.Errorf("build sandbox cmd: %w", err)
	}
	env := append(os.Environ(),
		"HIVE_ROOM_ID="+r.ID,
		"HIVE_AGENT_IMAGE="+img.Manifest.Name,
	)
	env = append(env, extraEnv...)
	cmd.Env = env
	if logFile != nil {
		cmd.Stderr = logFile
	} else {
		cmd.Stderr = os.Stderr
	}

	conn := agent.New(img.Manifest.Name, cmd)

	member := &Member{
		Image:         img.Ref(),
		Rank:          rk,
		QuotaOverride: opts.QuotaOverride,
		Mounts:        opts.Mounts,
		Conn:          conn,
		HiredAt:       time.Now(),
	}

	// Hooks install handlers BEFORE Start so the Agent can't race past them.
	if r.Hooks.RegisterAgentHandlers != nil {
		r.Hooks.RegisterAgentHandlers(r, member)
	}
	r.installCoreHandlers(member)

	if err := conn.Start(); err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}

	r.mu.Lock()
	r.members[img.Manifest.Name] = member
	r.mu.Unlock()
	r.router.Register(img.Manifest.Name, conn)

	if r.Hooks.OnStatus != nil {
		r.Hooks.OnStatus("agent_spawned", img.Manifest.Name, map[string]any{"rank": rk.Name})
	}

	// Reap when the Agent exits.
	go func() {
		<-conn.Done()
		r.router.Unregister(img.Manifest.Name)
		r.mu.Lock()
		delete(r.members, img.Manifest.Name)
		r.mu.Unlock()
		if r.Hooks.OnStatus != nil {
			info := map[string]any{}
			if err := conn.ExitErr(); err != nil {
				info["error"] = err.Error()
			}
			r.Hooks.OnStatus("agent_exited", img.Manifest.Name, info)
		}
	}()

	return member, nil
}

// Run dispatches a task to one Agent in the Room and waits for it to
// report task/done or task/error.
func (r *Room) Run(ctx context.Context, targetImage string, task json.RawMessage) (json.RawMessage, error) {
	m := r.Member(targetImage)
	if m == nil {
		return nil, fmt.Errorf("agent %s not found in room %s", targetImage, r.ID)
	}

	// A unique ID lets task/done correlate back if the Agent launches
	// multiple tasks (future; demo dispatches one at a time).
	taskID := fmt.Sprintf("%s-%d", r.ID, time.Now().UnixNano())
	done := make(chan json.RawMessage, 1)
	errCh := make(chan error, 1)

	m.Conn.Handle(rpc.MethodTaskDone, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p rpc.TaskDoneParams
		_ = json.Unmarshal(params, &p)
		if p.TaskID != taskID {
			// A stale completion from an earlier dispatch; ignore.
			return struct{}{}, nil
		}
		done <- p.Output
		return struct{}{}, nil
	})
	m.Conn.Handle(rpc.MethodTaskError, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p rpc.TaskErrorParams
		_ = json.Unmarshal(params, &p)
		if p.TaskID != taskID {
			return struct{}{}, nil
		}
		errCh <- fmt.Errorf("agent error (code=%d): %s", p.Code, p.Message)
		return struct{}{}, nil
	})

	_, err := m.Conn.Call(ctx, rpc.MethodTaskRun, rpc.TaskRunParams{
		TaskID: taskID,
		Input:  task,
	})
	if err != nil {
		return nil, fmt.Errorf("dispatch task/run: %w", err)
	}

	select {
	case out := <-done:
		return out, nil
	case e := <-errCh:
		return nil, e
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-m.Conn.Done():
		return nil, fmt.Errorf("agent %s exited before completing task", targetImage)
	}
}

// Stop shuts down all Agents and the router.
func (r *Room) Stop() error {
	r.mu.Lock()
	r.State = StateStopped
	ms := make([]*Member, 0, len(r.members))
	for _, m := range r.members {
		ms = append(ms, m)
	}
	r.mu.Unlock()

	for _, m := range ms {
		_ = m.Conn.Shutdown("room stopping")
	}
	// Give Agents a chance to drain.
	deadline := time.Now().Add(2 * time.Second)
	for _, m := range ms {
		remain := time.Until(deadline)
		if remain < 0 {
			remain = 0
		}
		select {
		case <-m.Conn.Done():
		case <-time.After(remain):
			_ = m.Conn.Kill()
			<-m.Conn.Done()
		}
	}
	r.cancel()
	r.router.Stop()
	close(r.done)
	return nil
}

// Router returns the Room's message router (used by the handler that
// implements peer/send on behalf of each Agent).
func (r *Room) Router() *router.Router { return r.router }

// installCoreHandlers wires per-Room handlers that every Agent gets:
// log forwarding, peer/send, task/done, task/error. Proxy-heavy handlers
// (fs/*, net/*, llm/*) come from Hooks.RegisterAgentHandlers in M4.
func (r *Room) installCoreHandlers(m *Member) {
	m.Conn.Handle(rpc.MethodLog, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p rpc.LogParams
		if err := json.Unmarshal(params, &p); err == nil && r.Hooks.OnLog != nil {
			r.Hooks.OnLog(m.Image.Name, p)
		}
		return struct{}{}, nil
	})

	m.Conn.Handle(rpc.MethodPeerSend, func(ctx context.Context, params json.RawMessage) (any, error) {
		var p rpc.PeerSendParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if err := r.router.Send(ctx, m.Image.Name, p.To, p.Payload); err != nil {
			return nil, err
		}
		return struct{}{}, nil
	})

	// task/done and task/error handlers are installed per-dispatch in Run();
	// leave sane no-ops here so orphan messages don't error-log on the Agent.
	noop := func(ctx context.Context, params json.RawMessage) (any, error) { return struct{}{}, nil }
	m.Conn.Handle(rpc.MethodTaskDone, noop)
	m.Conn.Handle(rpc.MethodTaskError, noop)
}
