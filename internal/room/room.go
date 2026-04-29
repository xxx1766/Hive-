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
	"io"
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
	// Model, when non-empty, overrides the manifest's default LLM model
	// for this hire (sets HIVE_MODEL env). Stored on the Member so
	// daemon-restart recovery can replay the original hire intent.
	Model string
	// QuotaOverride is set when Hivefile / CLI overrides the Rank's default
	// quota for this specific hire. nil ⇒ use Rank.Quota unchanged.
	QuotaOverride *rank.Quota
	// Mounts records the volumes bind-mounted into this Agent's sandbox.
	// The daemon uses it to construct the Agent's effective FS allow-list.
	Mounts []ns.Mount
	// Volumes preserves the original user-declared mount requests
	// (volume name + mountpoint + mode). The room itself only needs the
	// resolved Mounts; this field exists so the daemon can serialise the
	// hire intent for restart recovery without reverse-deriving names
	// from on-disk paths.
	Volumes []ipc.VolumeMountRef
	Conn    *agent.Conn
	HiredAt time.Time
	// Parent is the image name of the Agent that auto-hired this Member
	// (via SDK HireJunior). Empty for top-level hires (CLI, Hivefile).
	// Recovery preserves it so the subordinate-tree shape survives a
	// daemon restart.
	Parent string
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
	// PeerSendIntercept (when non-nil) is called BEFORE the router routes a
	// peer/send. Use it for Conversation round-cap enforcement: if the
	// hook returns non-nil, the peer/send fails with that error and the
	// message is never delivered. Pure precondition check; no side effects.
	PeerSendIntercept func(r *Room, from, to, convID string, payload json.RawMessage) error
	// PeerSendDelivered (when non-nil) is called AFTER the router has
	// successfully delivered a peer/recv to the target. Daemon uses it
	// to append the message to the Conversation transcript and publish
	// to the SSE bus. Fire-and-forget — return value ignored.
	PeerSendDelivered func(r *Room, from, to, convID string, payload json.RawMessage)
	// OnAgentExit fires after an Agent's process has been reaped and the
	// router has unregistered it. The daemon uses this to drop any
	// long-lived state keyed off the Conn — most notably event-bus
	// subscriptions, which would otherwise leak across the Agent's
	// lifetime — and to refund any unused subordinate quota back to
	// the parent (`m.Parent`). The Member pointer remains valid for
	// the duration of this call (room.go's exit goroutine holds a local
	// var) even though it's already been removed from r.members.
	// Called at most once per Conn.
	OnAgentExit func(r *Room, m *Member)
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
	// Every Room gets a workspace dir — it's the default inbox/outbox
	// bind-mounted into every hired Agent's /workspace and used as cwd
	// for ai_tool/invoke. Created eagerly so the daemon can mount it
	// without doing lazy-mkdir in the hot path.
	if err := os.MkdirAll(ipc.WorkspaceDir(id), 0o750); err != nil {
		return nil, fmt.Errorf("mkdir workspace: %w", err)
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
	// Model is the LLM model override (empty ⇒ keep manifest default).
	// Stored on Member so persistRoom can serialise it for recovery.
	Model         string
	QuotaOverride *rank.Quota // nil means "use Rank.Quota defaults"
	Mounts        []ns.Mount  // extra bind mounts inside the sandbox (volumes)
	// Volumes is the original wire-form list (name+mountpoint+mode) the
	// caller resolved into Mounts. Stored on Member purely so the daemon
	// can serialise it for restart recovery; the room itself doesn't act
	// on it.
	Volumes []ipc.VolumeMountRef
	// LogFile receives the Agent's stderr. Use a concrete *os.File if you
	// want the kernel to splice stderr directly (fastest path); use any
	// other io.WriteCloser (e.g. the daemon's rotating log) and os/exec
	// will insert its own copy goroutine — Hire closes the writer when
	// the Agent exits so ownership is unambiguous.
	LogFile  io.WriteCloser
	ExtraEnv []string
	// Parent is the image name of the auto-hiring Agent. Empty for
	// top-level hires (CLI / Hivefile). Threaded onto Member so the
	// daemon can serialise the subordinate tree for restart recovery
	// and surface it to UI / audit.
	Parent string
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

	cmd, initErrPipe, err := ns.NewAgentCommand(r.Rootfs, img.Dir, img.Manifest.Entry, opts.Mounts)
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

	conn := agent.New(img.Manifest.Name, cmd, initErrPipe)

	member := &Member{
		Image:         img.Ref(),
		Rank:          rk,
		Model:         opts.Model,
		QuotaOverride: opts.QuotaOverride,
		Mounts:        opts.Mounts,
		Volumes:       opts.Volumes,
		Conn:          conn,
		HiredAt:       time.Now(),
		Parent:        opts.Parent,
	}

	// Hooks install handlers BEFORE Start so the Agent can't race past them.
	if r.Hooks.RegisterAgentHandlers != nil {
		r.Hooks.RegisterAgentHandlers(r, member)
	}
	r.installCoreHandlers(member)

	if err := conn.Start(); err != nil {
		return nil, fmt.Errorf("start agent: %w", err)
	}

	// The sandbox init helper runs in the child between fork and the real
	// Agent exec; if it fails (pivot_root / mount errors) the child dies
	// immediately with a diagnostic written to FD 3. Block here until the
	// helper reports either success or a concrete reason so AgentHire
	// surfaces "pivot_root: operation not permitted" instead of the caller
	// later tripping over a generic "agent exited".
	if err := conn.WaitInit(); err != nil {
		_ = conn.Kill()
		<-conn.Done()
		return nil, fmt.Errorf("sandbox init: %w", err)
	}

	r.mu.Lock()
	r.members[img.Manifest.Name] = member
	r.mu.Unlock()
	r.router.Register(img.Manifest.Name, conn)

	if r.Hooks.OnStatus != nil {
		r.Hooks.OnStatus("agent_spawned", img.Manifest.Name, map[string]any{"rank": rk.Name})
	}

	// Reap when the Agent exits. cmd.Wait (inside conn.waitLoop) has
	// already joined os/exec's stderr-copy goroutine by the time Done
	// fires, so closing LogFile here is race-free — last bytes flushed
	// before the close hits.
	go func() {
		<-conn.Done()
		r.router.Unregister(img.Manifest.Name)
		r.mu.Lock()
		delete(r.members, img.Manifest.Name)
		r.mu.Unlock()
		if logFile != nil {
			_ = logFile.Close()
		}
		// Fire OnAgentExit *before* OnStatus so daemon-side cleanup
		// (event subscriptions etc.) happens while the Conn is still
		// the canonical identity for this Agent. OnStatus may surface
		// the exit to a CLI tail and we want bookkeeping done by then.
		if r.Hooks.OnAgentExit != nil {
			r.Hooks.OnAgentExit(r, member)
		}
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
// report task/done or task/error. Pass convID="" for ad-hoc runs;
// non-empty convID flags the dispatch as the entry-point of a
// Conversation so the runner can echo it back on outbound peer/send.
func (r *Room) Run(ctx context.Context, targetImage string, task json.RawMessage, convID string) (json.RawMessage, error) {
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
		ConvID: convID,
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
		// Conversation hooks: round-cap precheck, then post-success append.
		// Both are no-ops when the daemon hasn't installed them or when
		// p.ConvID == "" (peer messages outside any Conversation).
		if r.Hooks.PeerSendIntercept != nil {
			if err := r.Hooks.PeerSendIntercept(r, m.Image.Name, p.To, p.ConvID, p.Payload); err != nil {
				return nil, err
			}
		}
		if err := r.router.Send(ctx, m.Image.Name, p.To, p.ConvID, p.Payload); err != nil {
			return nil, err
		}
		if r.Hooks.PeerSendDelivered != nil {
			r.Hooks.PeerSendDelivered(r, m.Image.Name, p.To, p.ConvID, p.Payload)
		}
		return struct{}{}, nil
	})

	// task/done and task/error handlers are installed per-dispatch in Run();
	// leave sane no-ops here so orphan messages don't error-log on the Agent.
	noop := func(ctx context.Context, params json.RawMessage) (any, error) { return struct{}{}, nil }
	m.Conn.Handle(rpc.MethodTaskDone, noop)
	m.Conn.Handle(rpc.MethodTaskError, noop)
}
