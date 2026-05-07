package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// handleEvents serves Server-Sent Events for a single Room. Each event
// the daemon publishes through conversation.Bus is forwarded to every
// connected SSE client subscribed to that Room. The response uses HTTP
// streaming with no buffering so events arrive immediately.
//
// Wire format (text/event-stream):
//
//	event: <type>
//	data: <json>
//	\n
//
// Heartbeats every 15s keep proxies that drop idle connections (nginx,
// Cloudflare) from killing the stream while the room is quiet.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request, roomID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable nginx-style buffering

	ch, cancel := s.convBus.Subscribe(roomID)
	defer cancel()

	// Initial hello so the client knows the stream is live even when no
	// events follow for a while.
	fmt.Fprintf(w, "event: hello\ndata: {\"room_id\":%q,\"ts\":%q}\n\n",
		roomID, time.Now().UTC().Format(time.RFC3339Nano))
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case e, ok := <-ch:
			if !ok {
				return
			}
			data, err := json.Marshal(e)
			if err != nil {
				continue
			}
			// SSE specifies LF-only line endings.
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Type, data)
			flusher.Flush()
		case <-heartbeat.C:
			// Comment line — not seen by EventSource handlers but keeps
			// intermediaries from idling out the connection.
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
