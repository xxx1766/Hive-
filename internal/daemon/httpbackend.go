package daemon

import (
	"encoding/json"
	"errors"
	stdlog "log"

	"github.com/anne-x/hive/internal/httpapi"
	"github.com/anne-x/hive/internal/ipc"
	"github.com/anne-x/hive/internal/protocol"
)

// httpBackend adapts the Daemon to httpapi.Backend so the HTTP server
// gets a stable, narrow surface (avoids exposing daemon internals to a
// non-IPC client).
type httpBackend struct {
	d *Daemon
}

// ListRoomRefs is the read-only enumeration the UI uses to populate its
// room dropdown.
func (b *httpBackend) ListRoomRefs() []httpapi.RoomRef {
	b.d.mu.RLock()
	defer b.d.mu.RUnlock()
	out := make([]httpapi.RoomRef, 0, len(b.d.rooms))
	for _, r := range b.d.rooms {
		ref := r.Ref()
		out = append(out, httpapi.RoomRef{
			RoomID: ref.RoomID,
			Name:   ref.Name,
			State:  ref.State,
		})
	}
	return out
}

// RoomDetail returns the Members + the union of mounted volume names.
func (b *httpBackend) RoomDetail(roomID string) (httpapi.RoomDetail, bool) {
	b.d.mu.RLock()
	r := b.d.rooms[roomID]
	b.d.mu.RUnlock()
	if r == nil {
		return httpapi.RoomDetail{}, false
	}
	mems := r.Members()
	out := httpapi.RoomDetail{
		RoomID: roomID,
		Name:   r.Name,
		State:  string(r.State),
	}
	volSeen := map[string]bool{}
	for _, m := range mems {
		var vols []httpapi.VolumeMount
		for _, v := range m.Volumes {
			vols = append(vols, httpapi.VolumeMount{
				Name:       v.Name,
				Mode:       v.Mode,
				Mountpoint: v.Mountpoint,
			})
			if !volSeen[v.Name] {
				volSeen[v.Name] = true
				out.VolumeNames = append(out.VolumeNames, v.Name)
			}
		}
		out.Members = append(out.Members, httpapi.RoomMember{
			ImageName: m.Image.Name,
			Rank:      m.Rank.Name,
			State:     "running",
			Model:     m.Model,
			Volumes:   vols,
			Quota:     b.d.remainingQuota(r.ID, m),
		})
	}
	return out, true
}

// startHTTPAPI builds + boots the HTTP server. Hooks are direct
// adapters that re-enter the same handler logic the IPC handlers use,
// so HTTP and IPC paths produce indistinguishable side effects.
func (d *Daemon) startHTTPAPI() *httpapi.Server {
	hooks := httpapi.Hooks{
		CreateConversation: d.httpCreateConversation,
		StartConversation:  d.httpStartConversation,
		CancelConversation: d.httpCancelConversation,
	}
	srv := httpapi.NewServer(&httpBackend{d: d}, d.convStore, d.convBus, d.volumes, hooks)
	if err := srv.Start(); err != nil {
		// Failure to bind is non-fatal — IPC keeps working. Log loudly so
		// the user notices.
		stdlog.Printf("httpapi: failed to start: %v", err)
	}
	return srv
}

// httpCreateConversation feeds the same handleConversationCreate path
// the IPC dispatcher uses, but with HTTP-shaped inputs.
func (d *Daemon) httpCreateConversation(roomID string, p httpapi.ConvCreateInput) (string, error) {
	var rawInput json.RawMessage
	if p.Input != nil {
		b, err := json.Marshal(p.Input)
		if err != nil {
			return "", err
		}
		rawInput = b
	}
	params := ipc.ConversationCreateParams{
		RoomID:    roomID,
		Tag:       p.Tag,
		Target:    p.Target,
		Input:     rawInput,
		MaxRounds: p.MaxRounds,
	}
	body, _ := json.Marshal(params)
	res, err := d.handleConversationCreate(nil, body, nil)
	if err != nil {
		return "", unwrapErr(err)
	}
	r := res.(ipc.ConversationCreateResult)
	return r.ConvID, nil
}

func (d *Daemon) httpStartConversation(roomID, convID string) error {
	body, _ := json.Marshal(ipc.ConversationStartParams{RoomID: roomID, ConvID: convID})
	_, err := d.handleConversationStart(nil, body, nil)
	if err != nil {
		return unwrapErr(err)
	}
	return nil
}

func (d *Daemon) httpCancelConversation(roomID, convID, reason string) error {
	body, _ := json.Marshal(ipc.ConversationCancelParams{RoomID: roomID, ConvID: convID, Reason: reason})
	_, err := d.handleConversationCancel(nil, body, nil)
	if err != nil {
		return unwrapErr(err)
	}
	return nil
}

// unwrapErr peels protocol.Error so HTTP clients see the bare message.
// JSON-RPC framing is meaningless over HTTP; the message alone is what
// the UI surfaces in toasts.
func unwrapErr(err error) error {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return errors.New(pe.Message)
	}
	return err
}
