// Package daemon wires IPC handlers on top of the Room, Store and (M4)
// proxy services. It is the only place that knows about the full graph
// of dependencies — keeping other packages leaf-oriented.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/anne-x/hive/internal/conversation"
	"github.com/anne-x/hive/internal/eventbus"
	"github.com/anne-x/hive/internal/httpapi"
	"github.com/anne-x/hive/internal/image"
	"github.com/anne-x/hive/internal/ipc"
	"github.com/anne-x/hive/internal/ns"
	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/proxy/aitoolproxy"
	"github.com/anne-x/hive/internal/proxy/eventproxy"
	"github.com/anne-x/hive/internal/proxy/fsproxy"
	"github.com/anne-x/hive/internal/proxy/llmproxy"
	"github.com/anne-x/hive/internal/proxy/memproxy"
	"github.com/anne-x/hive/internal/proxy/netproxy"
	"github.com/anne-x/hive/internal/quota"
	"github.com/anne-x/hive/internal/rank"
	"github.com/anne-x/hive/internal/remote"
	"github.com/anne-x/hive/internal/room"
	"github.com/anne-x/hive/internal/roomstate"
	"github.com/anne-x/hive/internal/rpc"
	"github.com/anne-x/hive/internal/store"
	"github.com/anne-x/hive/internal/volume"
)

// Daemon is the top-level service. Fields are lazily initialised by New.
type Daemon struct {
	store          *store.Store
	ranks          *rank.Registry
	quota          *quota.Actor
	puller         *remote.Puller
	volumes        *volume.Manager
	events         *eventbus.Manager
	llmProvider    llmproxy.Provider
	aiToolProvider aitoolproxy.Provider

	// Conversation primitives: persisted multi-round Agent transcripts
	// + per-Room SSE bus for UI live updates.
	convStore *conversation.Store
	convBus   *conversation.Bus
	// convIndex resolves convID → ownerRoomID without scanning disk.
	// Built at startup from convStore.IndexByID(); updated by every
	// successful conversationCreate. Required for cross-Room peer_send
	// routing (the sender's Room doesn't always own the conv file).
	convIndex *convIndex
	// HTTP server for the embedded UI (non-fatal if it fails to bind).
	httpAPI *httpapi.Server

	quotaCtx    context.Context
	quotaCancel context.CancelFunc

	mu    sync.RWMutex
	rooms map[string]*room.Room

	// Live NotifyFunc for the currently-active room/run call, keyed by RoomID.
	notifyMu sync.RWMutex
	notifys  map[string]ipc.NotifyFunc

	// shuttingDown is set at the start of Shutdown so the post-exit
	// OnAgentExit hooks (fired by the reaper goroutines as Stop kills
	// each Agent) skip persistence — otherwise they'd race-overwrite
	// state.json with empty members and defeat recovery on next boot.
	shuttingDown bool
	shutMu       sync.RWMutex
}

// New builds a Daemon with the default on-disk locations.
// Starts the quota actor and picks an LLM provider (OpenAI if
// OPENAI_API_KEY is set, else Mock). Call Shutdown when done.
func New() (*Daemon, error) {
	if err := os.MkdirAll(ipc.ImagesDir(), 0o750); err != nil {
		return nil, fmt.Errorf("mkdir images: %w", err)
	}
	if err := os.MkdirAll(ipc.RoomsDir(), 0o750); err != nil {
		return nil, fmt.Errorf("mkdir rooms: %w", err)
	}

	// Pick LLM provider. If no API key, fall back to Mock so demo works offline.
	var prov llmproxy.Provider = llmproxy.MockProvider{}
	if oa := llmproxy.NewOpenAIFromEnv(); oa != nil {
		prov = oa
	}

	// Pick ai-tool provider (Claude Code if ANTHROPIC_API_KEY + `claude`
	// are present; Mock otherwise so `examples/coder` runs offline).
	var aiProv aitoolproxy.Provider = aitoolproxy.MockProvider{}
	if cc := aitoolproxy.NewClaudeCodeFromEnv(); cc != nil {
		aiProv = cc
	}

	q := quota.New(0)
	qCtx, qCancel := context.WithCancel(context.Background())
	go q.Run(qCtx)

	st := store.New(ipc.ImagesDir())

	volMgr, err := volume.New(ipc.VolumesDir())
	if err != nil {
		qCancel()
		return nil, fmt.Errorf("volumes: %w", err)
	}

	// Event bus: daemon-wide. Buses are created lazily on first
	// Subscribe/Publish; lifetime tied to qCtx so Shutdown takes them down.
	evtMgr := eventbus.New(qCtx)

	d := &Daemon{
		store:          st,
		ranks:          rank.DefaultRegistry(),
		quota:          q,
		puller:         remote.NewPuller(st),
		volumes:        volMgr,
		events:         evtMgr,
		llmProvider:    prov,
		aiToolProvider: aiProv,
		convStore:      conversation.NewStore(ipc.RoomsDir()),
		convBus:        conversation.NewBus(),
		convIndex:      newConvIndex(nil),
		quotaCtx:       qCtx,
		quotaCancel:    qCancel,
		rooms:          make(map[string]*room.Room),
		notifys:        make(map[string]ipc.NotifyFunc),
	}
	// Recover any Rooms persisted by a previous daemon. Best-effort: per-Room
	// failures (missing image, broken volume, agent that won't start) are
	// logged-and-skipped so a single bad Room can't keep the daemon down.
	// Must run before Register exposes RPC handlers — otherwise an early
	// agent/hire could race with the recovery writer for the same state.json.
	d.recoverRooms()

	// Conversation recovery: any conversation left in "active" was being
	// driven by a now-dead daemon process — there's no runner alive that
	// can converge it. Flip them to "interrupted" so the UI / CLI surfaces
	// the discontinuity instead of silently re-displaying them as live.
	if n, err := d.convStore.MarkActiveAsInterrupted(); err != nil {
		log.Printf("conversation recovery: %v", err)
	} else if n > 0 {
		log.Printf("conversation recovery: marked %d active conversation(s) as interrupted", n)
	}

	// Hydrate the in-memory convID→ownerRoomID index from disk so the
	// PeerSendForward hook can answer cross-Room routing queries
	// without scanning. New convs from this point are added via Set
	// from handleConversationCreate.
	if seed, err := d.convStore.IndexByID(); err != nil {
		log.Printf("conversation index: build failed: %v", err)
	} else {
		d.convIndex = newConvIndex(seed)
	}

	// HTTP UI server. Non-fatal if it can't bind — the IPC channel keeps
	// working. Address comes from HIVE_HTTP_ADDR (default 127.0.0.1:8910).
	d.httpAPI = d.startHTTPAPI()
	return d, nil
}

// Register installs all IPC handlers on srv.
func (d *Daemon) Register(srv *ipc.Server) {
	srv.Handle(ipc.MethodImageBuild, d.handleImageBuild)
	srv.Handle(ipc.MethodImageList, d.handleImageList)
	srv.Handle(ipc.MethodImagePull, d.handleImagePull)
	srv.Handle(ipc.MethodVolumeCreate, d.handleVolumeCreate)
	srv.Handle(ipc.MethodVolumeList, d.handleVolumeList)
	srv.Handle(ipc.MethodVolumeRemove, d.handleVolumeRemove)
	srv.Handle(ipc.MethodRoomInit, d.handleRoomInit)
	srv.Handle(ipc.MethodRoomList, d.handleRoomList)
	srv.Handle(ipc.MethodRoomStop, d.handleRoomStop)
	srv.Handle(ipc.MethodRoomTeam, d.handleRoomTeam)
	srv.Handle(ipc.MethodRoomLogs, d.handleRoomLogs)
	srv.Handle(ipc.MethodRoomRename, d.handleRoomRename)
	srv.Handle(ipc.MethodAgentHire, d.handleAgentHire)
	srv.Handle(ipc.MethodRoomRun, d.handleRoomRun)
	srv.Handle(ipc.MethodConversationCreate, d.handleConversationCreate)
	srv.Handle(ipc.MethodConversationStart, d.handleConversationStart)
	srv.Handle(ipc.MethodConversationList, d.handleConversationList)
	srv.Handle(ipc.MethodConversationGet, d.handleConversationGet)
	srv.Handle(ipc.MethodConversationCancel, d.handleConversationCancel)
}

// Shutdown stops every Room. Called when hived receives SIGTERM.
//
// The shuttingDown flag is set first so OnAgentExit hooks fired by the
// reaper goroutines as Agents die don't re-persist empty member lists
// over the snapshots recovery will read on next boot.
func (d *Daemon) Shutdown() {
	d.shutMu.Lock()
	d.shuttingDown = true
	d.shutMu.Unlock()

	// Stop accepting new HTTP requests first so in-flight UI calls
	// don't try to talk to a half-torn-down state.
	if d.httpAPI != nil {
		d.httpAPI.Stop()
	}

	d.mu.Lock()
	rms := make([]*room.Room, 0, len(d.rooms))
	for _, r := range d.rooms {
		rms = append(rms, r)
	}
	d.mu.Unlock()
	for _, r := range rms {
		_ = r.Stop()
	}
	if d.events != nil {
		d.events.Shutdown()
	}
	if d.quotaCancel != nil {
		d.quotaCancel()
	}
}

// isShuttingDown reports whether Shutdown has begun. Callers (notably
// the OnAgentExit hook) use this to skip recovery-relevant persistence
// while Agents are dying as part of an orderly daemon stop.
func (d *Daemon) isShuttingDown() bool {
	d.shutMu.RLock()
	defer d.shutMu.RUnlock()
	return d.shuttingDown
}

// installAgentProxies registers fs/net/llm Agent→Hive handlers on an Agent's
// Conn at hire time. Proxies enforce Rank and consume quota through the
// single quota.Actor; connection pooling for HTTP is hidden in netproxy.
func (d *Daemon) installAgentProxies(r *room.Room, m *room.Member) {
	// Install the merged (Rank default + override) quota. Quota keys
	// use m.Name (the in-room identity) so two members of the same
	// image don't share buckets — that's the entire point of aliasing.
	eff := m.EffectiveQuota()
	for k, v := range eff.Tokens {
		_, _ = d.quota.SetLimit(d.quotaCtx, quota.Key{
			RoomID: r.ID, Agent: m.Name, Resource: "tokens:" + k,
		}, v)
	}
	for k, v := range eff.APICalls {
		_, _ = d.quota.SetLimit(d.quotaCtx, quota.Key{
			RoomID: r.ID, Agent: m.Name, Resource: k,
		}, v)
	}

	// If the Agent has mounted volumes, build an effective Rank that
	// includes the mountpoints in its FSRead/FSWrite allow-list. The
	// proxy also needs redirect entries so agent-side paths resolve to
	// the real on-disk volume location (fsproxy runs in the daemon's
	// namespace, not the Agent's — bind-mounts are invisible to it
	// unless we hand them over explicitly).
	fsRank := m.Rank
	var fsMounts []fsproxy.MountRedirect
	if len(m.Mounts) > 0 {
		cloned := *m.Rank
		cloned.FSRead = append([]string{}, m.Rank.FSRead...)
		cloned.FSWrite = append([]string{}, m.Rank.FSWrite...)
		for _, mnt := range m.Mounts {
			cloned.FSRead = append(cloned.FSRead, mnt.Target)
			if !mnt.ReadOnly {
				cloned.FSWrite = append(cloned.FSWrite, mnt.Target)
			}
			fsMounts = append(fsMounts, fsproxy.MountRedirect{
				AgentPath: mnt.Target,
				HostPath:  mnt.Source,
			})
		}
		fsRank = &cloned
	}
	// Sort longest-prefix-first so nested mounts resolve correctly
	// (e.g. /shared/kb/docs beats /shared/kb).
	sort.SliceStable(fsMounts, func(i, j int) bool {
		return len(fsMounts[i].AgentPath) > len(fsMounts[j].AgentPath)
	})
	fs := &fsproxy.Proxy{RoomRootfs: r.Rootfs, Rank: fsRank, Mounts: fsMounts}
	net := &netproxy.Proxy{RoomID: r.ID, AgentName: m.Name, Rank: m.Rank, Quota: d.quota}
	llm := &llmproxy.Proxy{RoomID: r.ID, AgentName: m.Name, Rank: m.Rank, Quota: d.quota, Provider: d.llmProvider}
	mem := &memproxy.Proxy{
		RoomID:    r.ID,
		AgentName: m.Name,
		Rank:      m.Rank,
		Volumes:   d.volumes,
		RoomsDir:  ipc.RoomsDir(),
	}
	evt := &eventproxy.Proxy{
		RoomID:    r.ID,
		AgentName: m.Name,
		Rank:      m.Rank,
		Volumes:   d.volumes,
		Bus:       d.events,
		Conn:      m.Conn, // doubles as both delivery target and ownership identity
	}
	ait := &aitoolproxy.Proxy{
		RoomID:    r.ID,
		AgentName: m.Name,
		Rank:      m.Rank,
		Quota:     d.quota,
		Provider:  d.aiToolProvider,
		Workspace: ipc.WorkspaceDir(r.ID),
	}

	m.Conn.Handle(rpc.MethodFsRead, func(ctx context.Context, params json.RawMessage) (any, error) {
		return fs.Read(params)
	})
	m.Conn.Handle(rpc.MethodFsWrite, func(ctx context.Context, params json.RawMessage) (any, error) {
		return fs.Write(params)
	})
	m.Conn.Handle(rpc.MethodFsList, func(ctx context.Context, params json.RawMessage) (any, error) {
		return fs.List(params)
	})
	m.Conn.Handle(rpc.MethodNetFetch, func(ctx context.Context, params json.RawMessage) (any, error) {
		return net.Fetch(ctx, params)
	})
	m.Conn.Handle(rpc.MethodLLMComplete, func(ctx context.Context, params json.RawMessage) (any, error) {
		return llm.Complete(ctx, params)
	})

	m.Conn.Handle(rpc.MethodMemoryPut, func(ctx context.Context, params json.RawMessage) (any, error) {
		return mem.Put(params)
	})
	m.Conn.Handle(rpc.MethodMemoryGet, func(ctx context.Context, params json.RawMessage) (any, error) {
		return mem.Get(params)
	})
	m.Conn.Handle(rpc.MethodMemoryList, func(ctx context.Context, params json.RawMessage) (any, error) {
		return mem.List(params)
	})
	m.Conn.Handle(rpc.MethodMemoryDelete, func(ctx context.Context, params json.RawMessage) (any, error) {
		return mem.Delete(params)
	})

	m.Conn.Handle(rpc.MethodEventsPublish, func(ctx context.Context, params json.RawMessage) (any, error) {
		return evt.Publish(ctx, params)
	})
	m.Conn.Handle(rpc.MethodEventsSubscribe, func(ctx context.Context, params json.RawMessage) (any, error) {
		return evt.Subscribe(params)
	})
	m.Conn.Handle(rpc.MethodEventsUnsubscribe, func(ctx context.Context, params json.RawMessage) (any, error) {
		return evt.Unsubscribe(params)
	})

	m.Conn.Handle(rpc.MethodAIToolInvoke, func(ctx context.Context, params json.RawMessage) (any, error) {
		return ait.Invoke(ctx, params)
	})

	// hire/junior — manager+ rank only. Validates rank.CanHire, atomically
	// carves the requested quota out of the parent's remaining budget,
	// and installs the subordinate via the same hireFromConfig path that
	// agent/hire (CLI) and recovery use.
	m.Conn.Handle(rpc.MethodHireJunior, func(ctx context.Context, params json.RawMessage) (any, error) {
		return d.handleHireJunior(ctx, r, m, params)
	})
}

// ── image/* ──────────────────────────────────────────────────────────────

func (d *Daemon) handleImageBuild(ctx context.Context, params json.RawMessage, notify ipc.NotifyFunc) (any, error) {
	var p ipc.ImageBuildParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	abs, err := filepath.Abs(p.SourceDir)
	if err != nil {
		return nil, err
	}
	img, err := d.store.Put(abs)
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidManifest, err.Error())
	}
	return ipc.ImageBuildResult{
		Image: ipc.ImageRef{Name: img.Manifest.Name, Version: img.Manifest.Version},
		Path:  img.Dir,
	}, nil
}

func (d *Daemon) handleImagePull(ctx context.Context, params json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	var p ipc.ImagePullParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	ref, err := remote.ParseRef(p.URL)
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	img, err := d.puller.PullAgent(ctx, ref)
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInternal, err.Error())
	}
	return ipc.ImagePullResult{
		Image: ipc.ImageRef{Name: img.Manifest.Name, Version: img.Manifest.Version},
		Path:  img.Dir,
	}, nil
}

func (d *Daemon) handleImageList(ctx context.Context, _ json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	refs, err := d.store.List()
	if err != nil {
		return nil, err
	}
	out := make([]ipc.ImageRef, 0, len(refs))
	for _, r := range refs {
		out = append(out, ipc.ImageRef{Name: r.Name, Version: r.Version})
	}
	return ipc.ImageListResult{Images: out}, nil
}

// ── room/* ───────────────────────────────────────────────────────────────

func (d *Daemon) handleRoomInit(ctx context.Context, params json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	var p ipc.RoomInitParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	if p.Name == "" {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, "name is required")
	}

	roomID := fmt.Sprintf("%s-%d", p.Name, time.Now().Unix())
	r, err := d.createRoom(roomID, p.Name)
	if err != nil {
		return nil, err
	}
	// Persist immediately (members empty) so a daemon crash *before* the
	// first hire still leaves a recoverable Room rather than an orphan
	// rootfs/logs/workspace dir on disk.
	d.persistRoom(r)
	return ipc.RoomInitResult{RoomID: roomID, Rootfs: r.Rootfs}, nil
}

// createRoom is the shared in-memory Room constructor used by both
// handleRoomInit (fresh user request) and recoverRooms (daemon boot).
// It installs the per-Room Hooks and registers the Room in d.rooms.
func (d *Daemon) createRoom(roomID, name string) (*room.Room, error) {
	hooks := room.Hooks{
		OnLog: func(imageName string, lp rpc.LogParams) {
			d.streamLog(roomID, imageName, lp)
		},
		OnStatus: func(event, imageName string, info map[string]any) {
			d.streamStatus(roomID, event, imageName, info)
		},
		AuthPeerSend: func(r *room.Room, from, to string) error {
			// Same-Room peer send is always allowed in demo. Rank-based
			// peer policies can slot in here.
			return nil
		},
		PeerSendIntercept: func(r *room.Room, from, to, convID string, payload json.RawMessage) error {
			// No conv attached → ad-hoc peer message, skip Conversation
			// bookkeeping entirely. Existing peer/send semantics preserved.
			if convID == "" {
				return nil
			}
			// Owner Room may differ from sender's r.ID for cross-Room
			// convs — consult the index. Fall back to r.ID when the
			// index doesn't know (lets brand-new convs that hadn't
			// finished Create when this fired still resolve via the
			// sender's directory).
			ownerID := d.convIndex.Owner(convID)
			if ownerID == "" {
				ownerID = r.ID
			}
			c, err := d.convStore.Load(ownerID, convID)
			if err != nil {
				return protocol.NewError(protocol.ErrCodeInvalidParams,
					fmt.Sprintf("conversation %s not found", convID))
			}
			if c.Status.Terminal() {
				return protocol.NewError(protocol.ErrCodeInvalidParams,
					fmt.Sprintf("conversation %s is %s; cannot accept new messages", convID, c.Status))
			}
			if c.RoundCount >= c.MaxRounds {
				// Round cap hit. Cancel the conversation here so a single
				// reject is durable — subsequent peer/sends will see status
				// terminal and bounce on the check above. Caller-facing error
				// surfaces as a permission_denied so the agent's reply path
				// treats it like any other policy rejection.
				_, _ = d.convStore.Update(ownerID, convID, func(cc *conversation.Conversation) {
					cc.Status = conversation.StatusCancelled
					cc.Error = "round_cap"
					cc.FinishedAt = time.Now().UTC()
				})
				d.publishConvEvent(ownerID, convID, conversation.EventConvFinished, map[string]any{
					"reason": "round_cap", "max_rounds": c.MaxRounds,
				})
				return protocol.NewError(protocol.ErrCodePermissionDenied,
					fmt.Sprintf("conversation %s round cap (%d) exceeded", convID, c.MaxRounds))
			}
			return nil
		},
		PeerSendForward: d.peerSendForward,
		PeerSendDelivered: func(r *room.Room, from, to, convID string, payload json.RawMessage) {
			if convID == "" {
				return
			}
			ownerID := d.convIndex.Owner(convID)
			if ownerID == "" {
				ownerID = r.ID
			}
			updated, err := d.convStore.Append(ownerID, convID, conversation.Message{
				From:    from,
				To:      to,
				Kind:    conversation.KindPeer,
				Payload: payload,
			})
			if err != nil {
				log.Printf("conversation %s: append peer message failed: %v", convID, err)
				return
			}
			// Surface the new message on the SSE bus and as a daemon-wide
			// streaming notification so an active room/run subscriber also
			// sees the hop without polling.
			d.publishConvEvent(ownerID, convID, conversation.EventConvMessage, updated.Messages[len(updated.Messages)-1])
		},
		RegisterAgentHandlers: d.installAgentProxies,
		OnAgentExit: func(r *room.Room, m *room.Member) {
			// Drop every events/* subscription tied to this Conn so they
			// don't leak past the Agent's lifetime. Volume buses survive
			// (other Rooms may still hold subscriptions); only this
			// Conn's entries are removed.
			if d.events != nil {
				d.events.UnsubscribeNotifier(m.Conn)
			}
			// Refund-on-exit: if this Member was auto-hired by another
			// Agent (m.Parent != ""), return any unused portion of each
			// carved bucket to the parent's quota. The carve at hire time
			// was an atomic Consume on the parent; the inverse at exit
			// is an Uncharge. Bucket-by-bucket: refund = child.remaining
			// (i.e. carved limit minus what the child actually used).
			if m.Parent != "" {
				d.refundCarvesToParent(r.ID, m)
			}
			// Skip persistence while the daemon is shutting down — the
			// reapers fire as we kill Agents, but the existing on-disk
			// snapshot is exactly what we want recovery to read next
			// boot. Persisting now would erase the member list.
			if d.isShuttingDown() {
				return
			}
			// Live exit (Agent crashed or quit voluntarily): re-snapshot
			// so a future daemon restart won't try to re-hire an Agent
			// that's already gone. Member is already removed from
			// r.Members() by the reaper before this hook fires.
			d.mu.RLock()
			room := d.rooms[roomID]
			d.mu.RUnlock()
			if room != nil {
				d.persistRoom(room)
			}
		},
	}

	r, err := room.New(roomID, name, hooks)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.rooms[roomID] = r
	d.mu.Unlock()
	return r, nil
}

func (d *Daemon) handleRoomList(ctx context.Context, _ json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]ipc.RoomRef, 0, len(d.rooms))
	for _, r := range d.rooms {
		out = append(out, r.Ref())
	}
	// Go map iteration is randomized; sort by (Name, RoomID) so the CLI
	// and the UI sidebar show a stable order across calls. RoomID has
	// format "<name>-<unix>" so the tiebreak is chronological within a
	// name group.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].RoomID < out[j].RoomID
	})
	return ipc.RoomListResult{Rooms: out}, nil
}

func (d *Daemon) handleRoomStop(ctx context.Context, params json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	var p ipc.RoomStopParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	d.mu.Lock()
	r, ok := d.rooms[p.RoomID]
	if ok {
		delete(d.rooms, p.RoomID)
	}
	d.mu.Unlock()
	if !ok {
		return nil, protocol.NewError(protocol.ErrCodeRoomNotFound, "room not found: "+p.RoomID)
	}
	if err := r.Stop(); err != nil {
		return nil, err
	}
	// Reap the same-Room broadcast bus too (Agents have already exited
	// via OnAgentExit, but the empty bus would otherwise linger).
	if d.events != nil {
		d.events.RemoveScope(eventbus.RoomKey(p.RoomID))
	}
	// Explicit user stop ⇒ drop persisted state so a future daemon
	// restart doesn't resurrect the Room. (Daemon Shutdown, by contrast,
	// keeps state.json — that's exactly what recovery is for.)
	if err := roomstate.Delete(ipc.RoomsDir(), p.RoomID); err != nil {
		log.Printf("hived: delete state for room %s: %v", p.RoomID, err)
	}
	return struct{}{}, nil
}

// handleRoomRename mutates a Room's display Name. RoomID stays untouched
// (it's a stable router key + dir name); only the human-facing label
// changes. Persisted via the existing snapshotFor → roomstate.Save path
// so the rename survives daemon restart.
func (d *Daemon) handleRoomRename(ctx context.Context, params json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	var p ipc.RoomRenameParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, "name is required")
	}
	if len(name) > 64 {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, "name too long (max 64 chars)")
	}
	d.mu.Lock()
	r, ok := d.rooms[p.RoomID]
	if ok {
		r.Name = name
	}
	d.mu.Unlock()
	if !ok {
		return nil, protocol.NewError(protocol.ErrCodeRoomNotFound, "room not found: "+p.RoomID)
	}
	d.persistRoom(r)
	return ipc.RoomRenameResult{RoomID: r.ID, Name: name}, nil
}

func (d *Daemon) handleRoomTeam(ctx context.Context, params json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	var p ipc.RoomTeamParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	d.mu.RLock()
	r, ok := d.rooms[p.RoomID]
	d.mu.RUnlock()
	if !ok {
		return nil, protocol.NewError(protocol.ErrCodeRoomNotFound, "room not found: "+p.RoomID)
	}
	mems := r.Members()
	out := make([]ipc.TeamMember, 0, len(mems))
	for _, m := range mems {
		out = append(out, ipc.TeamMember{
			Name:      m.Name,
			ImageName: m.Image.Name,
			Rank:      m.Rank.Name,
			State:     "running",
			Quota:     d.remainingQuota(r.ID, m),
		})
	}
	return ipc.RoomTeamResult{RoomID: p.RoomID, Members: out}, nil
}

// ── volume/* ─────────────────────────────────────────────────────────────

func (d *Daemon) handleVolumeCreate(ctx context.Context, params json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	var p ipc.VolumeCreateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	v, err := d.volumes.Create(p.Name)
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	return ipc.VolumeRef{Name: v.Name, Path: v.Path}, nil
}

func (d *Daemon) handleVolumeList(ctx context.Context, _ json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	vs, err := d.volumes.List()
	if err != nil {
		return nil, err
	}
	out := make([]ipc.VolumeRef, 0, len(vs))
	for _, v := range vs {
		out = append(out, ipc.VolumeRef{Name: v.Name, Path: v.Path})
	}
	return ipc.VolumeListResult{Volumes: out}, nil
}

func (d *Daemon) handleVolumeRemove(ctx context.Context, params json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	var p ipc.VolumeRemoveParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	if err := d.volumes.Remove(p.Name); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	// Tear down the matching event-bus scope so any in-flight publishes
	// fail fast and stale subscribers can re-subscribe cleanly if the
	// Volume is later recreated. Idempotent if no bus existed.
	if d.events != nil {
		d.events.RemoveScope(eventbus.VolumeKey(p.Name))
	}
	return struct{}{}, nil
}

// handleRoomLogs reads the persisted per-Agent stderr log files under
// <RoomsDir>/<roomID>/logs/ and returns their contents in one shot.
// No tailing / follow in MVP — snapshots only.
func (d *Daemon) handleRoomLogs(ctx context.Context, params json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	var p ipc.RoomLogsParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	d.mu.RLock()
	_, ok := d.rooms[p.RoomID]
	d.mu.RUnlock()
	if !ok {
		// Allow logs for stopped rooms too, but only if the dir still
		// exists on disk — Stop currently leaves them for post-mortem.
		logsDir := filepath.Join(ipc.RoomsDir(), p.RoomID, "logs")
		if _, err := os.Stat(logsDir); err != nil {
			return nil, protocol.NewError(protocol.ErrCodeRoomNotFound, "room not found: "+p.RoomID)
		}
	}

	logsDir := filepath.Join(ipc.RoomsDir(), p.RoomID, "logs")
	entries, err := os.ReadDir(logsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return ipc.RoomLogsResult{RoomID: p.RoomID}, nil
		}
		return nil, err
	}

	out := ipc.RoomLogsResult{RoomID: p.RoomID}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".stderr.log") {
			continue
		}
		agent := strings.TrimSuffix(e.Name(), ".stderr.log")
		if p.Agent != "" && agent != p.Agent {
			continue
		}
		path := filepath.Join(logsDir, e.Name())
		contents, err := os.ReadFile(path)
		if err != nil {
			contents = []byte(fmt.Sprintf("(read error: %v)", err))
		}
		out.Entries = append(out.Entries, ipc.RoomLogEntry{
			Agent: agent, Path: path, Contents: string(contents),
		})
	}
	if p.Agent != "" && len(out.Entries) == 0 {
		return nil, protocol.NewError(protocol.ErrCodeAgentNotFound,
			fmt.Sprintf("no log for agent %q in room %s", p.Agent, p.RoomID))
	}
	return out, nil
}

// remainingQuota produces a display-only snapshot of what's left per resource.
// Unlimited resources are omitted; shows {resource: remaining} as ints.
func (d *Daemon) remainingQuota(roomID string, m *room.Member) map[string]any {
	out := map[string]any{}
	eff := m.EffectiveQuota()
	for k := range eff.Tokens {
		res, err := d.quota.Remaining(d.quotaCtx, quota.Key{
			RoomID: roomID, Agent: m.Name, Resource: "tokens:" + k,
		})
		if err == nil && !res.Unlimited {
			out["tokens:"+k] = res.Remaining
		}
	}
	for k := range eff.APICalls {
		res, err := d.quota.Remaining(d.quotaCtx, quota.Key{
			RoomID: roomID, Agent: m.Name, Resource: k,
		})
		if err == nil && !res.Unlimited {
			out[k] = res.Remaining
		}
	}
	return out
}

// ── agent/hire ───────────────────────────────────────────────────────────

func (d *Daemon) handleAgentHire(ctx context.Context, params json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	var p ipc.AgentHireParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	d.mu.RLock()
	r, ok := d.rooms[p.RoomID]
	d.mu.RUnlock()
	if !ok {
		return nil, protocol.NewError(protocol.ErrCodeRoomNotFound, "room not found: "+p.RoomID)
	}
	m, err := d.hireFromConfig(r, hireConfig{
		Image:     p.Image,
		RankName:  p.RankName,
		Model:     p.Model,
		QuotaOver: p.QuotaOverr,
		Volumes:   p.Volumes,
	})
	if err != nil {
		return nil, err
	}
	d.persistRoom(r)
	return ipc.AgentHireResult{
		Member: ipc.TeamMember{
			Name:      m.Name,
			ImageName: m.Image.Name,
			Rank:      m.Rank.Name,
			State:     "running",
		},
	}, nil
}

// hireConfig is the wire-form input shared by handleAgentHire and the
// daemon-restart recovery path. Same shape as ipc.AgentHireParams minus
// RoomID — the caller already resolved the Room.
type hireConfig struct {
	Image     ipc.ImageRef
	RankName  string
	Model     string // overrides manifest.Model (HIVE_MODEL); empty ⇒ keep manifest's
	QuotaOver json.RawMessage
	Volumes   []ipc.VolumeMountRef
	// Parent is the auto-hiring Agent's name (empty ⇒ top-level CLI /
	// Hivefile hire). Threaded through to room.Member so the
	// subordinate tree survives daemon restart via roomstate.MemberSnap.
	Parent string
	// Name is the in-room identity for this hire (overrides
	// img.Manifest.Name). Empty ⇒ use the image name. Allows the same
	// image to be hired multiple times under distinct aliases.
	Name string
}

// hireFromConfig is the single entry point that turns a hireConfig into
// a running Member. Both the agent/hire RPC and recoverOne(boot) use it
// — that's the whole point of the extraction: recovery hires Agents via
// exactly the same code path the user's first hire took.
func (d *Daemon) hireFromConfig(r *room.Room, cfg hireConfig) (*room.Member, error) {
	img, err := d.store.Get(image.Ref{Name: cfg.Image.Name, Version: cfg.Image.Version})
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeImageNotFound, err.Error())
	}
	rankName := cfg.RankName
	if rankName == "" {
		rankName = img.Manifest.Rank
	}
	rk, err := d.ranks.Get(rankName)
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeRankViolation, err.Error())
	}

	// Capability pre-flight: requires[] from manifest must each be in rk.Capabilities().
	// Surfaces "rank intern doesn't grant llm" at hire time instead of the first
	// llm/complete call inside a task.
	for _, req := range img.Manifest.Capabilities.Requires {
		if !rk.HasCapability(req) {
			return nil, protocol.NewError(protocol.ErrCodeRankViolation,
				fmt.Sprintf("rank %q does not grant required capability %q (manifest requires: %v)",
					rk.Name, req, img.Manifest.Capabilities.Requires))
		}
	}
	// Resolve the in-room name early — it drives the log filename so
	// the OS-level file collision (two members of the same image
	// trying to open the same .stderr.log) is avoided in the alias
	// case.
	memberName := cfg.Name
	if memberName == "" {
		memberName = img.Manifest.Name
	}
	// Per-agent stderr log for easier debugging. Capped + rotated so
	// long-running Agents don't fill the disk (see logrotate.go). Declared
	// as the interface type so an open failure leaves logFile as a real
	// nil (avoiding the typed-nil trap through HireOpts.LogFile).
	logPath := filepath.Join(ipc.RoomsDir(), r.ID, "logs", memberName+".stderr.log")
	var logFile io.WriteCloser
	if f, err := openRotatingLog(logPath, logMaxBytes()); err == nil {
		logFile = f
	}

	preparedImg, extraEnv, err := d.prepareImageByKind(img, cfg.Model)
	if err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, protocol.NewError(protocol.ErrCodeInternal, err.Error())
	}

	// Parse the Hivefile / CLI quota override, if present.
	var quotaOverride *rank.Quota
	if len(cfg.QuotaOver) > 0 && string(cfg.QuotaOver) != "null" {
		var ov ipc.QuotaOverride
		if err := json.Unmarshal(cfg.QuotaOver, &ov); err != nil {
			if logFile != nil {
				_ = logFile.Close()
			}
			return nil, protocol.NewError(protocol.ErrCodeInvalidParams,
				fmt.Sprintf("parse quota override: %v", err))
		}
		q := rank.Quota{Tokens: ov.Tokens, APICalls: ov.APICalls}
		quotaOverride = &q
	}

	// Every Agent gets its Room's workspace bind-mounted as /workspace.
	// This is the default inbox/outbox and the cwd for ai_tool/invoke.
	// Declared first so Hivefile-listed volumes never collide with it
	// via their mountpoint (a user can't accidentally mount another vol
	// at /workspace — that'd shadow the default one).
	mounts := []ns.Mount{{
		Source:   ipc.WorkspaceDir(r.ID),
		Target:   "/workspace",
		ReadOnly: false,
	}}

	// Resolve requested volumes. Fail fast with a clear error if a name
	// doesn't exist — better than letting the Agent crash at fs_read time.
	for _, v := range cfg.Volumes {
		vol, err := d.volumes.Get(v.Name)
		if err != nil {
			if logFile != nil {
				_ = logFile.Close()
			}
			return nil, protocol.NewError(protocol.ErrCodeInvalidParams,
				fmt.Sprintf("volume %q: %v — create with `hive volume create %s`", v.Name, err, v.Name))
		}
		mode := v.Mode
		if mode == "" {
			mode = "ro"
		}
		mounts = append(mounts, ns.Mount{
			Source:   vol.Path,
			Target:   v.Mountpoint,
			ReadOnly: mode == "ro",
		})
	}

	m, err := r.Hire(preparedImg, room.HireOpts{
		Rank:          rk,
		Model:         cfg.Model,
		QuotaOverride: quotaOverride,
		Mounts:        mounts,
		Volumes:       cfg.Volumes,
		LogFile:       logFile,
		ExtraEnv:      extraEnv,
		Parent:        cfg.Parent,
		Name:          memberName,
	})
	if err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, err
	}
	return m, nil
}

// ── room/run (streams) ───────────────────────────────────────────────────

func (d *Daemon) handleRoomRun(ctx context.Context, params json.RawMessage, notify ipc.NotifyFunc) (any, error) {
	var p ipc.RoomRunParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	d.mu.RLock()
	r, ok := d.rooms[p.RoomID]
	d.mu.RUnlock()
	if !ok {
		return nil, protocol.NewError(protocol.ErrCodeRoomNotFound, "room not found: "+p.RoomID)
	}
	target := p.Target
	if target == "" {
		mems := r.Members()
		if len(mems) == 0 {
			return nil, fmt.Errorf("no agents hired in room %s", p.RoomID)
		}
		target = mems[0].Image.Name
	}

	d.notifyMu.Lock()
	d.notifys[p.RoomID] = notify
	d.notifyMu.Unlock()
	defer func() {
		d.notifyMu.Lock()
		delete(d.notifys, p.RoomID)
		d.notifyMu.Unlock()
	}()

	out, err := r.Run(ctx, target, p.Task, "")
	if err != nil {
		return nil, err
	}
	return ipc.RoomRunResult{Output: out}, nil
}

// streamLog / streamStatus fire notifications to whoever is currently
// subscribed to this Room's run (if any).
func (d *Daemon) streamLog(roomID, imageName string, lp rpc.LogParams) {
	d.notifyMu.RLock()
	notify := d.notifys[roomID]
	d.notifyMu.RUnlock()
	if notify == nil {
		return
	}
	notify(ipc.NotifyRoomLog, ipc.RoomLogNotification{
		RoomID:    roomID,
		ImageName: imageName,
		Level:     lp.Level,
		Msg:       lp.Msg,
		Fields:    lp.Fields,
		Time:      time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (d *Daemon) streamStatus(roomID, event, imageName string, info map[string]any) {
	d.notifyMu.RLock()
	notify := d.notifys[roomID]
	d.notifyMu.RUnlock()
	if notify == nil {
		return
	}
	notify(ipc.NotifyRoomStatus, ipc.RoomStatusNotification{
		RoomID: roomID,
		Event:  event,
		Image:  imageName,
		Info:   info,
		Time:   time.Now().UTC().Format(time.RFC3339Nano),
	})
}

// ── persistence + recovery ───────────────────────────────────────────────

// persistRoom snapshots the Room's current member list to disk. Called
// after every state-changing operation (room create, agent hire, agent
// exit). Persistence failures are logged but never bubble up — recovery
// is opportunistic and shouldn't break user-visible operations.
func (d *Daemon) persistRoom(r *room.Room) {
	snap := snapshotFor(r)
	if err := roomstate.Save(ipc.RoomsDir(), r.ID, snap); err != nil {
		log.Printf("hived: persist room %s: %v", r.ID, err)
	}
}

// aliasOrEmpty returns name when it differs from imageName, otherwise
// "". Keeps the on-disk MemberSnap.Name field zero-valued for the
// common (non-aliased) case so state.json doesn't bloat with redundant
// data.
func aliasOrEmpty(name, imageName string) string {
	if name == imageName {
		return ""
	}
	return name
}

// snapshotFor builds a Snapshot from the Room's live state. Members are
// reduced to the wire-form bits hireFromConfig consumes — image ref,
// rank name, quota override (re-marshalled), volume mounts. Anything
// process-bound (Conn, router, sub_ids, quota counters) is discarded.
func snapshotFor(r *room.Room) *roomstate.Snapshot {
	members := r.Members()
	out := make([]roomstate.MemberSnap, 0, len(members))
	for _, m := range members {
		ms := roomstate.MemberSnap{
			Image:    ipc.ImageRef{Name: m.Image.Name, Version: m.Image.Version},
			RankName: m.Rank.Name,
			Model:    m.Model,
			Volumes:  m.Volumes,
			HiredAt:  m.HiredAt,
			Parent:   m.Parent,
			// Persist Name only when it differs from the image name —
			// avoids bloating state.json for the common (top-level
			// CLI / Hivefile) case where they're identical.
			Name: aliasOrEmpty(m.Name, m.Image.Name),
		}
		if m.QuotaOverride != nil {
			// Re-encode through the wire shape so on-disk state stays
			// stable across rank.Quota refactors.
			raw, err := json.Marshal(ipc.QuotaOverride{
				Tokens:   m.QuotaOverride.Tokens,
				APICalls: m.QuotaOverride.APICalls,
			})
			if err == nil {
				ms.QuotaOver = raw
			}
		}
		out = append(out, ms)
	}
	// Stable order makes diffs and tests deterministic. Sort by member
	// name (the in-room identity) so two members of the same image
	// have a predictable ordering by their alias.
	sort.Slice(out, func(i, j int) bool {
		ai := out[i].Name
		if ai == "" {
			ai = out[i].Image.Name
		}
		aj := out[j].Name
		if aj == "" {
			aj = out[j].Image.Name
		}
		return ai < aj
	})
	return &roomstate.Snapshot{
		Version: roomstate.CurrentVersion,
		Name:    r.Name,
		Members: out,
	}
}

// recoverRooms walks state.json files left by a prior daemon and re-runs
// hire for each persisted member. Best-effort: a Room or member that
// can't be recovered (missing image, broken volume, agent fails to
// start) is logged and skipped — never fatal. State files are NOT
// rewritten here, so the next normal mutation reflects reality and
// failures can be inspected on disk.
func (d *Daemon) recoverRooms() {
	loaded, err := roomstate.LoadAll(ipc.RoomsDir())
	if err != nil {
		log.Printf("hived: roomstate.LoadAll: %v", err)
		return
	}
	// Stable order makes startup logs and tests deterministic.
	sort.Slice(loaded, func(i, j int) bool { return loaded[i].RoomID < loaded[j].RoomID })
	for _, snap := range loaded {
		if err := d.recoverOne(snap); err != nil {
			log.Printf("hived: recover room %s: %v", snap.RoomID, err)
		}
	}
}

// recoverOne resurrects a single Room from its persisted snapshot.
// Missing/broken members are skipped with a warning so the rest of the
// Room still comes back; an error here is reserved for "couldn't even
// build the Room object" (mkdir failed, etc).
func (d *Daemon) recoverOne(snap roomstate.Loaded) error {
	r, err := d.createRoom(snap.RoomID, snap.Name)
	if err != nil {
		return fmt.Errorf("createRoom: %w", err)
	}
	for _, ms := range snap.Members {
		_, err := d.hireFromConfig(r, hireConfig{
			Image:     ms.Image,
			RankName:  ms.RankName,
			Model:     ms.Model,
			QuotaOver: ms.QuotaOver,
			Volumes:   ms.Volumes,
			Parent:    ms.Parent,
			Name:      ms.Name,
		})
		if err != nil {
			log.Printf("hived: recover %s/%s: %v", snap.RoomID, ms.Image.Name, err)
			continue
		}
	}
	return nil
}
