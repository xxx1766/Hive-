// Package httpapi exposes Hive's daemon state over a small HTTP+SSE
// surface so a browser UI can render Rooms, Conversations, and Volumes
// without going through the JSON-RPC IPC channel. The server runs in
// the same process as hived; it has direct access to the same Store
// and Bus the IPC handlers use, so there's no marshalling overhead and
// no risk of a state divergence.
//
// Listen address is taken from HIVE_HTTP_ADDR (default 127.0.0.1:8910).
// Setting it to "" disables the HTTP listener entirely — useful in CI
// or when running multiple daemons on the same host.
//
// Wire surface (all paths under /api/*):
//
//	GET  /api/rooms                              list rooms with conv counts
//	GET  /api/rooms/{id}                         room detail (members + conv summary + volumes)
//	GET  /api/rooms/{id}/conversations           full list of conversation summaries
//	GET  /api/rooms/{id}/conversations/{cid}     full conversation transcript
//	POST /api/rooms/{id}/conversations           create planned conversation
//	POST /api/rooms/{id}/conversations/{cid}/start
//	POST /api/rooms/{id}/conversations/{cid}/cancel
//	GET  /api/rooms/{id}/events                  Server-Sent Events stream
//	GET  /api/volumes                            list named volumes
//	GET  /api/volumes/{name}/tree                file tree under a volume
//	GET  /api/volumes/{name}/file?p=<rel>        file contents (text only, 1MB cap)
//	GET  /                                       embedded SPA (UI)
package httpapi

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/anne-x/hive/internal/conversation"
	"github.com/anne-x/hive/internal/volume"
)

// DefaultAddr is the listen address when HIVE_HTTP_ADDR is unset.
const DefaultAddr = "127.0.0.1:8910"

// EnvAddr names the env var that overrides the listen address. Empty
// string disables the listener. Anything else (e.g. ":8910" for all
// interfaces) is passed straight to net.Listen.
const EnvAddr = "HIVE_HTTP_ADDR"

//go:embed ui
var uiFS embed.FS

// Backend is the slice of daemon state the HTTP server reads. Keeping
// it an interface (vs. importing *daemon.Daemon) breaks the import
// cycle: daemon → httpapi → daemon would not compile.
type Backend interface {
	// ListRoomRefs returns one entry per Room: id + display name + state
	// label ("idle"/"running"/etc). The HTTP layer enriches with conv
	// counts before responding.
	ListRoomRefs() []RoomRef
	// RoomDetail bundles the on-disk snapshot for a single room. Returns
	// false if the room is not currently known to the daemon.
	RoomDetail(roomID string) (RoomDetail, bool)
}

// RoomRef is a compact room descriptor returned by ListRoomRefs. Mirrors
// ipc.RoomRef but lives here so httpapi doesn't import ipc.
type RoomRef struct {
	RoomID string `json:"room_id"`
	Name   string `json:"name"`
	State  string `json:"state"`
}

// RoomDetail is the deeper view of one room.
type RoomDetail struct {
	RoomID  string       `json:"room_id"`
	Name    string       `json:"name"`
	State   string       `json:"state"`
	Members []RoomMember `json:"members"`
	// VolumeNames are the named volumes mounted into any of this Room's
	// agents. UI uses these to populate the volume browser pane.
	VolumeNames []string `json:"volume_names"`
}

// RoomMember mirrors the team-member view but shaped for the HTTP UI.
type RoomMember struct {
	// Name is the in-room identity (defaults to ImageName, possibly
	// aliased on hire_junior — see hire/junior IPC). UI uses Name as
	// the row label and falls back to ImageName when they're equal.
	Name      string         `json:"name"`
	ImageName string         `json:"image"`
	Rank      string         `json:"rank"`
	State     string         `json:"state"`
	Model     string         `json:"model,omitempty"`
	// Parent is the in-room name of the auto-hiring Agent (empty for
	// top-level CLI / Hivefile hires). UI uses it to render the
	// subordinate tree.
	Parent  string         `json:"parent,omitempty"`
	Volumes []VolumeMount  `json:"volumes,omitempty"`
	Quota   map[string]any `json:"quota,omitempty"`
}

type VolumeMount struct {
	Name       string `json:"name"`
	Mode       string `json:"mode"`
	Mountpoint string `json:"mountpoint"`
}

// Server hosts the HTTP API.
type Server struct {
	addr      string
	backend   Backend
	convStore *conversation.Store
	convBus   *conversation.Bus
	volumes   *volume.Manager

	// Conversation control (create/start/cancel) is plumbed back through
	// the daemon so it shares lifecycle code with the IPC handlers.
	createConv func(roomID string, p ConvCreateInput) (string, error)
	startConv  func(roomID, convID string) error
	cancelConv func(roomID, convID, reason string) error

	httpServer *http.Server
	listener   net.Listener
}

// ConvCreateInput is the shape of POST /api/rooms/{id}/conversations.
type ConvCreateInput struct {
	Tag       string `json:"tag,omitempty"`
	Target    string `json:"target"`
	Input     any    `json:"input,omitempty"`
	MaxRounds int    `json:"max_rounds,omitempty"`
}

// Hooks bundles the daemon-side dispatch entry points. They mirror the
// IPC handlers so that HTTP + IPC paths share the same lifecycle code.
type Hooks struct {
	CreateConversation func(roomID string, p ConvCreateInput) (string, error)
	StartConversation  func(roomID, convID string) error
	CancelConversation func(roomID, convID, reason string) error
}

// NewServer wires up an HTTP server. It does not start listening — call
// Start to spawn the goroutine.
func NewServer(b Backend, store *conversation.Store, bus *conversation.Bus, vol *volume.Manager, hooks Hooks) *Server {
	addr := os.Getenv(EnvAddr)
	if addr == "" && os.Getenv(EnvAddr+"_DISABLED") == "" {
		addr = DefaultAddr
	}
	return &Server{
		addr:       addr,
		backend:    b,
		convStore:  store,
		convBus:    bus,
		volumes:    vol,
		createConv: hooks.CreateConversation,
		startConv:  hooks.StartConversation,
		cancelConv: hooks.CancelConversation,
	}
}

// Start begins listening. Returns nil immediately when HIVE_HTTP_ADDR is
// the empty string (HTTP disabled). Otherwise the listener and server
// run in the background; the returned error is the bind error if any.
// Call Stop on shutdown.
func (s *Server) Start() error {
	if s.addr == "" {
		log.Printf("httpapi: HIVE_HTTP_ADDR=\"\" — HTTP UI disabled")
		return nil
	}
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		return err
	}
	s.listener = ln

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpServer = &http.Server{
		Handler:           withCORS(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("httpapi: listening on http://%s", s.listener.Addr())

	go func() {
		if err := s.httpServer.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("httpapi: serve: %v", err)
		}
	}()
	return nil
}

// Stop gracefully shuts the HTTP server down. Idempotent.
func (s *Server) Stop() {
	if s.httpServer == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = s.httpServer.Shutdown(ctx)
}

// registerRoutes attaches handlers. Routing uses path prefixes since
// net/http's stdlib mux doesn't support pattern variables — keeping the
// dispatcher explicit avoids a third-party router dep.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/rooms", s.handleRooms)
	mux.HandleFunc("/api/rooms/", s.handleRoomScoped) // /api/rooms/{id}/...
	mux.HandleFunc("/api/volumes", s.handleVolumes)
	mux.HandleFunc("/api/volumes/", s.handleVolumeScoped)

	// SPA — embed.FS rooted at "ui".
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		log.Printf("httpapi: ui embed: %v", err)
		return
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))
}

// withCORS adds permissive CORS headers — the UI is served from the
// same origin in normal use, but this helps when developing the SPA
// against a local file or a different port.
func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// stripPrefix returns the trailing portion of url after `prefix` and
// reports whether the prefix matched. Returns "" if URL == prefix.
func stripPrefix(url, prefix string) (string, bool) {
	if !strings.HasPrefix(url, prefix) {
		return "", false
	}
	return strings.TrimPrefix(url, prefix), true
}
