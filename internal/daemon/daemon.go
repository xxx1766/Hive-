// Package daemon wires IPC handlers on top of the Room, Store and (M4)
// proxy services. It is the only place that knows about the full graph
// of dependencies — keeping other packages leaf-oriented.
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/anne-x/hive/internal/image"
	"github.com/anne-x/hive/internal/ipc"
	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/proxy/fsproxy"
	"github.com/anne-x/hive/internal/proxy/llmproxy"
	"github.com/anne-x/hive/internal/proxy/netproxy"
	"github.com/anne-x/hive/internal/quota"
	"github.com/anne-x/hive/internal/rank"
	"github.com/anne-x/hive/internal/remote"
	"github.com/anne-x/hive/internal/room"
	"github.com/anne-x/hive/internal/rpc"
	"github.com/anne-x/hive/internal/store"
)

// Daemon is the top-level service. Fields are lazily initialised by New.
type Daemon struct {
	store       *store.Store
	ranks       *rank.Registry
	quota       *quota.Actor
	puller      *remote.Puller
	llmProvider llmproxy.Provider

	quotaCtx    context.Context
	quotaCancel context.CancelFunc

	mu    sync.RWMutex
	rooms map[string]*room.Room

	// Live NotifyFunc for the currently-active room/run call, keyed by RoomID.
	notifyMu sync.RWMutex
	notifys  map[string]ipc.NotifyFunc
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

	q := quota.New(0)
	qCtx, qCancel := context.WithCancel(context.Background())
	go q.Run(qCtx)

	st := store.New(ipc.ImagesDir())

	return &Daemon{
		store:       st,
		ranks:       rank.DefaultRegistry(),
		quota:       q,
		puller:      remote.NewPuller(st),
		llmProvider: prov,
		quotaCtx:    qCtx,
		quotaCancel: qCancel,
		rooms:       make(map[string]*room.Room),
		notifys:     make(map[string]ipc.NotifyFunc),
	}, nil
}

// Register installs all IPC handlers on srv.
func (d *Daemon) Register(srv *ipc.Server) {
	srv.Handle(ipc.MethodImageBuild, d.handleImageBuild)
	srv.Handle(ipc.MethodImageList, d.handleImageList)
	srv.Handle(ipc.MethodImagePull, d.handleImagePull)
	srv.Handle(ipc.MethodRoomInit, d.handleRoomInit)
	srv.Handle(ipc.MethodRoomList, d.handleRoomList)
	srv.Handle(ipc.MethodRoomStop, d.handleRoomStop)
	srv.Handle(ipc.MethodRoomTeam, d.handleRoomTeam)
	srv.Handle(ipc.MethodAgentHire, d.handleAgentHire)
	srv.Handle(ipc.MethodRoomRun, d.handleRoomRun)
}

// Shutdown stops every Room. Called when hived receives SIGTERM.
func (d *Daemon) Shutdown() {
	d.mu.Lock()
	rms := make([]*room.Room, 0, len(d.rooms))
	for _, r := range d.rooms {
		rms = append(rms, r)
	}
	d.mu.Unlock()
	for _, r := range rms {
		_ = r.Stop()
	}
	if d.quotaCancel != nil {
		d.quotaCancel()
	}
}

// installAgentProxies registers fs/net/llm Agent→Hive handlers on an Agent's
// Conn at hire time. Proxies enforce Rank and consume quota through the
// single quota.Actor; connection pooling for HTTP is hidden in netproxy.
func (d *Daemon) installAgentProxies(r *room.Room, m *room.Member) {
	// Install manifest-default quotas. Hivefile/CLI overrides would go
	// here too; M4 ships without overrides (default-only is already
	// enough to demo quota isolation).
	for k, v := range m.Rank.Quota.Tokens {
		_, _ = d.quota.SetLimit(d.quotaCtx, quota.Key{
			RoomID: r.ID, Agent: m.Image.Name, Resource: "tokens:" + k,
		}, v)
	}
	for k, v := range m.Rank.Quota.APICalls {
		_, _ = d.quota.SetLimit(d.quotaCtx, quota.Key{
			RoomID: r.ID, Agent: m.Image.Name, Resource: k,
		}, v)
	}

	fs := &fsproxy.Proxy{RoomRootfs: r.Rootfs, Rank: m.Rank}
	net := &netproxy.Proxy{RoomID: r.ID, AgentName: m.Image.Name, Rank: m.Rank, Quota: d.quota}
	llm := &llmproxy.Proxy{RoomID: r.ID, AgentName: m.Image.Name, Rank: m.Rank, Quota: d.quota, Provider: d.llmProvider}

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
		RegisterAgentHandlers: d.installAgentProxies,
	}

	r, err := room.New(roomID, p.Name, hooks)
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	d.rooms[roomID] = r
	d.mu.Unlock()

	return ipc.RoomInitResult{RoomID: roomID, Rootfs: r.Rootfs}, nil
}

func (d *Daemon) handleRoomList(ctx context.Context, _ json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]ipc.RoomRef, 0, len(d.rooms))
	for _, r := range d.rooms {
		out = append(out, r.Ref())
	}
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
	return struct{}{}, nil
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
			ImageName: m.Image.Name,
			Rank:      m.Rank.Name,
			State:     "running",
			Quota:     d.remainingQuota(r.ID, m),
		})
	}
	return ipc.RoomTeamResult{RoomID: p.RoomID, Members: out}, nil
}

// remainingQuota produces a display-only snapshot of what's left per resource.
// Unlimited resources are omitted; shows {resource: remaining} as ints.
func (d *Daemon) remainingQuota(roomID string, m *room.Member) map[string]any {
	out := map[string]any{}
	for k := range m.Rank.Quota.Tokens {
		res, err := d.quota.Remaining(d.quotaCtx, quota.Key{
			RoomID: roomID, Agent: m.Image.Name, Resource: "tokens:" + k,
		})
		if err == nil && !res.Unlimited {
			out["tokens:"+k] = res.Remaining
		}
	}
	for k := range m.Rank.Quota.APICalls {
		res, err := d.quota.Remaining(d.quotaCtx, quota.Key{
			RoomID: roomID, Agent: m.Image.Name, Resource: k,
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
	img, err := d.store.Get(image.Ref{Name: p.Image.Name, Version: p.Image.Version})
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeImageNotFound, err.Error())
	}
	rankName := p.RankName
	if rankName == "" {
		rankName = img.Manifest.Rank
	}
	rk, err := d.ranks.Get(rankName)
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeRankViolation, err.Error())
	}
	// Per-agent stderr log for easier debugging.
	logPath := filepath.Join(ipc.RoomsDir(), p.RoomID, "logs", img.Manifest.Name+".stderr.log")
	logFile, _ := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)

	preparedImg, extraEnv, err := d.prepareImageByKind(img)
	if err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, protocol.NewError(protocol.ErrCodeInternal, err.Error())
	}
	m, err := r.Hire(preparedImg, rk, logFile, extraEnv...)
	if err != nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		return nil, err
	}
	return ipc.AgentHireResult{
		Member: ipc.TeamMember{
			ImageName: m.Image.Name,
			Rank:      m.Rank.Name,
			State:     "running",
		},
	}, nil
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

	out, err := r.Run(ctx, target, p.Task)
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
