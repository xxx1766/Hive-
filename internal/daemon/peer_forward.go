package daemon

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/anne-x/hive/internal/conversation"
	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/room"
)

// peerSendForward is the cross-Room routing pivot installed as
// room.Hooks.PeerSendForward on every Room. Returns (handled=true,
// nil) when it has dispatched the message itself; (false, nil) means
// "default local-router path is fine, take it" and (handled=true,
// err) means "I tried to handle it and failed".
//
// The decision tree:
//
//   - convID == ""                      → ad-hoc, local path
//   - conv unknown to index             → local path (best-effort fallback)
//   - conv has no Members declared      → legacy local conv, local path
//   - all Members in sender's Room      → local path (degenerate cross-Room)
//   - target name not in Members        → reject (policy violation)
//   - target's Room is sender's Room    → local path
//   - target's Room is another active   → dispatch via target's router
//                                         + transcript append + event publish
//   - target's Room is stopped/missing  → reject
//
// Symmetric in both directions: A→B uses B.Router(); B→A uses
// A.Router(). The append always lands on the conv's owner Room
// directory regardless of who's sending. That keeps the on-disk
// transcript single-sourced and the SSE bus event scoped to one
// room id, so existing UI subscribers don't need to chase split
// transcripts.
func (d *Daemon) peerSendForward(r *room.Room, from, to, convID string, payload json.RawMessage) (bool, error) {
	if convID == "" {
		return false, nil
	}
	ownerID := d.convIndex.Owner(convID)
	if ownerID == "" {
		// Conv unknown to the daemon index — let the local path try
		// (PeerSendIntercept already validated it loaded from r.ID,
		// so if we got past that, the conv lives in r.ID's dir).
		return false, nil
	}
	c, err := d.convStore.Load(ownerID, convID)
	if err != nil {
		return true, protocol.NewError(protocol.ErrCodeInternal,
			fmt.Sprintf("peer_send: conv %s load failed: %v", convID, err))
	}
	if !c.IsCrossRoom() {
		// Members may exist but all in one Room — fall through to the
		// local router (which already knows the target by name).
		return false, nil
	}

	target := c.FindMember(to)
	if target == nil {
		return true, protocol.NewError(protocol.ErrCodePermissionDenied,
			fmt.Sprintf("peer_send to=%s: not in conversation %s members", to, convID))
	}
	if target.RoomID == r.ID {
		// Target lives in the sender's Room — the local router
		// already has it registered. Falling through preserves the
		// existing fast path and AuthPeerSend invariants.
		return false, nil
	}

	d.mu.RLock()
	targetRoom, ok := d.rooms[target.RoomID]
	d.mu.RUnlock()
	if !ok {
		return true, protocol.NewError(protocol.ErrCodeInvalidParams,
			fmt.Sprintf("peer_send to=%s/%s: target room %s is not active",
				target.RoomID, target.AgentName, target.RoomID))
	}

	// Dispatch via the target's home-Room router. Use d.quotaCtx so
	// the call survives the originating IPC's context (the sender's
	// peer/send is a one-shot — we don't want a slow target Room to
	// outlive the daemon, but we do want it to outlive the agent's
	// individual RPC timeout).
	if err := targetRoom.Router().Send(d.quotaCtx, from, target.AgentName, convID, payload); err != nil {
		return true, fmt.Errorf("peer_send forward to %s/%s: %w",
			target.RoomID, target.AgentName, err)
	}

	// Append to transcript under the conv's owner Room. Mirrors what
	// PeerSendDelivered would have done — kept here because the
	// non-local dispatch took us off that hook's path.
	updated, err := d.convStore.Append(ownerID, convID, conversation.Message{
		From:    from,
		To:      to,
		Kind:    conversation.KindPeer,
		Payload: payload,
	})
	if err != nil {
		log.Printf("conversation %s: cross-room append failed: %v", convID, err)
		return true, nil
	}
	d.publishConvEvent(ownerID, convID, conversation.EventConvMessage,
		updated.Messages[len(updated.Messages)-1])
	return true, nil
}
