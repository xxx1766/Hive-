package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/anne-x/hive/internal/conversation"
	"github.com/anne-x/hive/internal/ipc"
	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/room"
)

// handleConversationCreate plans a new Conversation. The Conversation is
// persisted in status=planned; nothing is dispatched yet — the UI / CLI
// follows up with conversation/start when the user is ready.
func (d *Daemon) handleConversationCreate(ctx context.Context, params json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	var p ipc.ConversationCreateParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	if p.RoomID == "" || p.Target == "" {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, "room_id and target are required")
	}
	d.mu.RLock()
	r := d.rooms[p.RoomID]
	d.mu.RUnlock()
	if r == nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, fmt.Sprintf("room %s not found", p.RoomID))
	}
	if r.Member(p.Target) == nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams,
			fmt.Sprintf("agent %s not hired in room %s", p.Target, p.RoomID))
	}

	c := &conversation.Conversation{
		ID:            conversation.NewID(),
		RoomID:        p.RoomID,
		Tag:           p.Tag,
		Status:        conversation.StatusPlanned,
		InitialTarget: p.Target,
		InitialInput:  p.Input,
		MaxRounds:     p.MaxRounds,
	}
	if err := d.convStore.Create(c); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInternal, err.Error())
	}
	d.convIndex.Set(c.ID, c.RoomID)
	d.publishConvEvent(p.RoomID, c.ID, conversation.EventConvCreated, c.Summarize())
	return ipc.ConversationCreateResult{
		ConvID: c.ID,
		Status: string(c.Status),
	}, nil
}

// handleConversationStart dispatches a planned Conversation: sends the
// initial input to the target agent with conv_id propagated. The dispatch
// happens in a background goroutine — we return as soon as the conv flips
// to active so the UI can stream events while the agent works.
func (d *Daemon) handleConversationStart(ctx context.Context, params json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	var p ipc.ConversationStartParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	d.mu.RLock()
	r := d.rooms[p.RoomID]
	d.mu.RUnlock()
	if r == nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, fmt.Sprintf("room %s not found", p.RoomID))
	}
	c, err := d.convStore.Load(p.RoomID, p.ConvID)
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	if c.Status != conversation.StatusPlanned {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams,
			fmt.Sprintf("conversation %s is %s; only 'planned' can be started", p.ConvID, c.Status))
	}
	if r.Member(c.InitialTarget) == nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams,
			fmt.Sprintf("initial target %s not hired in room %s", c.InitialTarget, p.RoomID))
	}

	updated, err := d.convStore.Update(p.RoomID, p.ConvID, func(cc *conversation.Conversation) {
		cc.Status = conversation.StatusActive
		cc.StartedAt = time.Now().UTC()
	})
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInternal, err.Error())
	}
	// Record the initial input as message m1 (round 0) so the transcript
	// reads as a self-contained sequence.
	if _, aerr := d.convStore.Append(p.RoomID, p.ConvID, conversation.Message{
		From:    "",
		To:      c.InitialTarget,
		Kind:    conversation.KindTaskInput,
		Payload: c.InitialInput,
	}); aerr != nil {
		log.Printf("conversation %s: initial input append failed: %v", p.ConvID, aerr)
	}
	d.publishConvEvent(p.RoomID, p.ConvID, conversation.EventConvStarted, updated.Summarize())

	// Dispatch in a background goroutine so the IPC call returns
	// immediately. The room.Run call blocks until task/done.
	go d.runConversation(r, c)

	return ipc.ConversationStartResult{
		ConvID: p.ConvID,
		Status: string(conversation.StatusActive),
	}, nil
}

// runConversation drives the initial dispatch and updates the Conversation's
// terminal state when the runner reports task/done or task/error. Subsequent
// peer hops between agents update RoundCount via the PeerSendDelivered hook.
func (d *Daemon) runConversation(r *room.Room, c *conversation.Conversation) {
	// Use a fresh context tied to the daemon's quota lifetime so the run
	// keeps going even after the originating IPC call returned.
	ctx := d.quotaCtx
	out, err := r.Run(ctx, c.InitialTarget, c.InitialInput, c.ID)

	// Daemon-shutdown short-circuit: if r.Run unblocked because the
	// agent's Conn was torn down by Shutdown (not by anything specific
	// to this conversation), don't write any status. The post-restart
	// recovery sweep will pick it up via MarkActiveAsInterrupted and
	// flip it to interrupted — the right semantic.
	if d.isShuttingDown() {
		return
	}

	// If a peer interception cancelled the conversation mid-run (round_cap),
	// don't overwrite the cancelled status — only flip when we're still active.
	cur, lerr := d.convStore.Load(r.ID, c.ID)
	if lerr != nil {
		log.Printf("conversation %s: post-run load failed: %v", c.ID, lerr)
		return
	}
	if cur.Status.Terminal() {
		// Terminal already (round_cap or external cancel) — only emit the
		// finished event once.
		d.publishConvEvent(r.ID, c.ID, conversation.EventConvFinished, cur.Summarize())
		return
	}

	if err != nil {
		_, _ = d.convStore.Update(r.ID, c.ID, func(cc *conversation.Conversation) {
			cc.Status = conversation.StatusFailed
			cc.Error = err.Error()
			cc.FinishedAt = time.Now().UTC()
		})
	} else {
		_, _ = d.convStore.Update(r.ID, c.ID, func(cc *conversation.Conversation) {
			cc.Status = conversation.StatusDone
			cc.FinalAnswer = out
			cc.FinishedAt = time.Now().UTC()
			// Append the final answer as a transcript message so a UI
			// rendering the timeline shows it inline.
			cc.Messages = append(cc.Messages, conversation.Message{
				ID:      fmt.Sprintf("m%d", len(cc.Messages)+1),
				ConvID:  c.ID,
				From:    c.InitialTarget,
				To:      "",
				Kind:    conversation.KindTaskOutput,
				Payload: out,
				TS:      time.Now().UTC(),
				Round:   cc.RoundCount,
			})
		})
	}
	finished, _ := d.convStore.Load(r.ID, c.ID)
	if finished != nil {
		d.publishConvEvent(r.ID, c.ID, conversation.EventConvFinished, finished.Summarize())
	}
}

// handleConversationList returns compact summaries (no transcripts) so the
// UI can render dozens of conversations cheaply.
func (d *Daemon) handleConversationList(ctx context.Context, params json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	var p ipc.ConversationListParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	convs, err := d.convStore.ListByRoom(p.RoomID)
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInternal, err.Error())
	}
	out := make([]ipc.ConversationSummary, 0, len(convs))
	for _, c := range convs {
		out = append(out, summaryToWire(c.Summarize()))
	}
	return ipc.ConversationListResult{RoomID: p.RoomID, Conversations: out}, nil
}

// handleConversationGet returns the full Conversation as raw JSON so the
// UI gets exactly the on-disk schema with no lossy reshape.
func (d *Daemon) handleConversationGet(ctx context.Context, params json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	var p ipc.ConversationGetParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	c, err := d.convStore.Load(p.RoomID, p.ConvID)
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInternal, err.Error())
	}
	return ipc.ConversationGetResult{Conversation: raw}, nil
}

// handleConversationCancel flips an active or planned Conversation to
// cancelled. Idempotent on terminal states. Does NOT kill the agent —
// the runner will keep going until its current task completes; the
// conv simply stops accepting new round-counted hops.
func (d *Daemon) handleConversationCancel(ctx context.Context, params json.RawMessage, _ ipc.NotifyFunc) (any, error) {
	var p ipc.ConversationCancelParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	c, err := d.convStore.Load(p.RoomID, p.ConvID)
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	if c.Status.Terminal() {
		// Already terminal — return current status, don't error.
		return ipc.ConversationCancelResult{ConvID: p.ConvID, Status: string(c.Status)}, nil
	}
	reason := p.Reason
	if reason == "" {
		reason = "user_cancel"
	}
	updated, err := d.convStore.Update(p.RoomID, p.ConvID, func(cc *conversation.Conversation) {
		cc.Status = conversation.StatusCancelled
		cc.Error = reason
		cc.FinishedAt = time.Now().UTC()
	})
	if err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInternal, err.Error())
	}
	d.publishConvEvent(p.RoomID, p.ConvID, conversation.EventConvFinished, updated.Summarize())
	return ipc.ConversationCancelResult{ConvID: p.ConvID, Status: string(updated.Status)}, nil
}

// publishConvEvent fans the event out to:
//   - the per-Room conversation.Bus (UI subscribers via SSE)
//   - the active room/run notifier (if any) — keeps existing CLI streams
//     alive even when a Conversation is the underlying driver
func (d *Daemon) publishConvEvent(roomID, convID string, t conversation.EventType, payload any) {
	now := time.Now().UTC()
	d.convBus.Publish(conversation.Event{
		Type:    t,
		RoomID:  roomID,
		ConvID:  convID,
		Payload: payload,
		TS:      now,
	})

	d.notifyMu.RLock()
	notify := d.notifys[roomID]
	d.notifyMu.RUnlock()
	if notify == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		raw = json.RawMessage(`null`)
	}
	notify(ipc.NotifyConversationEvt, ipc.ConversationEventNotification{
		Type:    string(t),
		RoomID:  roomID,
		ConvID:  convID,
		Payload: raw,
		Time:    now.Format(time.RFC3339Nano),
	})
}

// summaryToWire copies a conversation.Summary into ipc.ConversationSummary,
// formatting timestamps as RFC3339 strings (the wire shape).
func summaryToWire(s conversation.Summary) ipc.ConversationSummary {
	out := ipc.ConversationSummary{
		ID:            s.ID,
		RoomID:        s.RoomID,
		Tag:           s.Tag,
		Status:        string(s.Status),
		InitialTarget: s.InitialTarget,
		MaxRounds:     s.MaxRounds,
		RoundCount:    s.RoundCount,
		MessageCount:  s.MessageCount,
		CreatedAt:     s.CreatedAt.Format(time.RFC3339Nano),
	}
	if !s.StartedAt.IsZero() {
		out.StartedAt = s.StartedAt.Format(time.RFC3339Nano)
	}
	if !s.FinishedAt.IsZero() {
		out.FinishedAt = s.FinishedAt.Format(time.RFC3339Nano)
	}
	return out
}
