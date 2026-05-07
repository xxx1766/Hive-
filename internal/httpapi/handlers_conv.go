package httpapi

import (
	"encoding/json"
	"net/http"
)

// routeRoomConversations dispatches the /conversations subtree.
//
//	tail == []                          GET list / POST create
//	tail == [convID]                    GET full record
//	tail == [convID, "start"]           POST
//	tail == [convID, "cancel"]          POST
func (s *Server) routeRoomConversations(w http.ResponseWriter, r *http.Request, roomID string, tail []string) {
	switch {
	case len(tail) == 0:
		switch r.Method {
		case http.MethodGet:
			s.listConversations(w, roomID)
		case http.MethodPost:
			s.createConversation(w, r, roomID)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	case len(tail) == 1:
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.getConversation(w, roomID, tail[0])

	case len(tail) == 2 && tail[1] == "start":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.startConversation(w, roomID, tail[0])

	case len(tail) == 2 && tail[1] == "cancel":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.cancelConversation(w, r, roomID, tail[0])

	default:
		http.NotFound(w, r)
	}
}

func (s *Server) listConversations(w http.ResponseWriter, roomID string) {
	convs, err := s.convStore.ListByRoom(roomID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]any, 0, len(convs))
	for _, c := range convs {
		out = append(out, c.Summarize())
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createConversation(w http.ResponseWriter, r *http.Request, roomID string) {
	var p ConvCreateInput
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if p.Target == "" {
		http.Error(w, "target is required", http.StatusBadRequest)
		return
	}
	convID, err := s.createConv(roomID, p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conv_id": convID, "status": "planned"})
}

func (s *Server) getConversation(w http.ResponseWriter, roomID, convID string) {
	c, err := s.convStore.Load(roomID, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) startConversation(w http.ResponseWriter, roomID, convID string) {
	if err := s.startConv(roomID, convID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conv_id": convID, "status": "active"})
}

func (s *Server) cancelConversation(w http.ResponseWriter, r *http.Request, roomID, convID string) {
	var p struct {
		Reason string `json:"reason,omitempty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&p) // body optional
	if err := s.cancelConv(roomID, convID, p.Reason); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conv_id": convID, "status": "cancelled"})
}
