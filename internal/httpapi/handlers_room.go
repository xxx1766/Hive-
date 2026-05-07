package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
)

// handleRooms responds to GET /api/rooms — list rooms with conversation
// counts overlaid on the basic refs from the daemon.
func (s *Server) handleRooms(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type roomResp struct {
		RoomRef
		ConversationCounts map[string]int `json:"conv_counts"`
	}
	refs := s.backend.ListRoomRefs()
	out := make([]roomResp, 0, len(refs))
	for _, r := range refs {
		counts := map[string]int{}
		convs, _ := s.convStore.ListByRoom(r.RoomID)
		for _, c := range convs {
			counts[string(c.Status)]++
		}
		out = append(out, roomResp{RoomRef: r, ConversationCounts: counts})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRoomScoped routes /api/rooms/{id}/... to the right sub-handler.
// Layout:
//
//	/api/rooms/{id}                              GET  RoomDetail
//	/api/rooms/{id}/rename                       POST mutate display Name
//	/api/rooms/{id}/conversations                GET  list / POST create
//	/api/rooms/{id}/conversations/{cid}          GET  full record
//	/api/rooms/{id}/conversations/{cid}/start    POST
//	/api/rooms/{id}/conversations/{cid}/cancel   POST
//	/api/rooms/{id}/events                       GET  SSE stream
func (s *Server) handleRoomScoped(w http.ResponseWriter, r *http.Request) {
	tail, ok := stripPrefix(r.URL.Path, "/api/rooms/")
	if !ok || tail == "" {
		http.NotFound(w, r)
		return
	}
	parts := strings.SplitN(tail, "/", 4)
	roomID := parts[0]
	if roomID == "" {
		http.NotFound(w, r)
		return
	}

	switch {
	case len(parts) == 1:
		// /api/rooms/{id}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.serveRoomDetail(w, roomID)

	case len(parts) == 2 && parts[1] == "rename":
		s.handleRoomRename(w, r, roomID)

	case len(parts) >= 2 && parts[1] == "conversations":
		s.routeRoomConversations(w, r, roomID, parts[2:])

	case len(parts) == 2 && parts[1] == "events":
		s.handleEvents(w, r, roomID)

	default:
		http.NotFound(w, r)
	}
}

// handleRoomRename POST /api/rooms/{id}/rename
//
//	body: {"name": "<new name>"}
//	200:  {"room_id": "...", "name": "<new name>"}
func (s *Server) handleRoomRename(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var p struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if s.renameRoom == nil {
		http.Error(w, "rename not wired", http.StatusInternalServerError)
		return
	}
	if err := s.renameRoom(roomID, p.Name); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"room_id": roomID, "name": strings.TrimSpace(p.Name)})
}

// serveRoomDetail returns Members + Volumes + summary block.
func (s *Server) serveRoomDetail(w http.ResponseWriter, roomID string) {
	d, ok := s.backend.RoomDetail(roomID)
	if !ok {
		http.Error(w, "room not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// writeJSON marshals v as JSON with the given status. On marshal error
// returns 500 with the underlying error so the caller sees it.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		// Header already written; best we can do is log via the writer.
		_, _ = w.Write([]byte(`{"error":"encode failed"}`))
	}
}
