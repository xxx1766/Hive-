// Package eventproxy handles Agent events/* requests — the real-time
// counterpart to the persistent memory/* family.
//
// Scope resolution mirrors memproxy.resolveDir exactly so Agent authors
// only learn one rule:
//
//	""        → same-Room broadcast (events go to subscribers in this Room only)
//	"<name>"  → cross-Room broadcast via the named Volume (must already
//	            exist, just like memory/* with a Volume scope)
//
// The Volume need not have anything written into its memory/ dir for
// events to flow — events are ephemeral and never touch the file system.
// Volumes serve here as the named, opt-in cross-Room namespace; treating
// them as the addressing primitive keeps the security/ACL story the same
// as memory/* (knowing the Volume name = capability).
//
// Access control: Rank.MemoryAllowed is the single gate today. If a
// future Rank wants to permit memory/* but block events, split this into
// Rank.EventsAllowed — the change is one-line here.
package eventproxy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anne-x/hive/internal/eventbus"
	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/rank"
	"github.com/anne-x/hive/internal/rpc"
	"github.com/anne-x/hive/internal/volume"
)

// Proxy is constructed per-Agent at hire time. Conn is the Agent's
// daemon-side connection; it doubles as both the delivery target for
// events/recv and the owner identity for unsubscribe ACL.
type Proxy struct {
	RoomID    string
	AgentName string
	Rank      *rank.Rank
	Volumes   *volume.Manager
	Bus       *eventbus.Manager
	Conn      eventbus.Notifier
}

// ── Handlers ─────────────────────────────────────────────────────────────

func (p *Proxy) Publish(ctx context.Context, params json.RawMessage) (any, error) {
	if err := p.gate(); err != nil {
		return nil, err
	}
	var r rpc.EventsPublishParams
	if err := json.Unmarshal(params, &r); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	key, err := p.resolveScopeKey(r.Scope)
	if err != nil {
		return nil, err
	}
	bus := p.Bus.GetOrCreate(key)
	if err := bus.Publish(ctx, eventbus.Envelope{
		Scope:     r.Scope,
		FromRoom:  p.RoomID,
		FromAgent: p.AgentName,
		Payload:   r.Payload,
	}); err != nil {
		return nil, asProtocolErr(err)
	}
	return struct{}{}, nil
}

func (p *Proxy) Subscribe(params json.RawMessage) (any, error) {
	if err := p.gate(); err != nil {
		return nil, err
	}
	var r rpc.EventsSubscribeParams
	if err := json.Unmarshal(params, &r); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	key, err := p.resolveScopeKey(r.Scope)
	if err != nil {
		return nil, err
	}
	bus := p.Bus.GetOrCreate(key)
	subID := p.Bus.NewSubID()
	bus.Subscribe(eventbus.Subscriber{
		SubID:     subID,
		Notifier:  p.Conn,
		AgentName: p.AgentName,
		RoomID:    p.RoomID,
	})
	return rpc.EventsSubscribeResult{SubID: subID}, nil
}

func (p *Proxy) Unsubscribe(params json.RawMessage) (any, error) {
	if err := p.gate(); err != nil {
		return nil, err
	}
	var r rpc.EventsUnsubscribeParams
	if err := json.Unmarshal(params, &r); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	if r.SubID == "" {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, "events: sub_id is required")
	}
	found, owned := p.Bus.UnsubscribeOwned(r.SubID, p.Conn)
	switch {
	case !found:
		// Idempotent: already gone (or never existed) is success.
		return struct{}{}, nil
	case !owned:
		return nil, protocol.ErrPermissionDenied("events: sub_id belongs to another agent")
	}
	return struct{}{}, nil
}

// ── helpers ─────────────────────────────────────────────────────────────

func (p *Proxy) gate() error {
	if !p.Rank.MemoryAllowed {
		return protocol.ErrPermissionDenied("rank " + p.Rank.Name + " cannot use events/*")
	}
	return nil
}

func (p *Proxy) resolveScopeKey(scope string) (string, error) {
	if scope == "" {
		return eventbus.RoomKey(p.RoomID), nil
	}
	if _, err := p.Volumes.Get(scope); err != nil {
		return "", protocol.NewError(protocol.ErrCodeInvalidParams,
			fmt.Sprintf("events: scope %q: %v — create with `hive volume create %s`", scope, err, scope))
	}
	return eventbus.VolumeKey(scope), nil
}

// asProtocolErr lifts plain errors (e.g. "bus stopped") into the canonical
// JSON-RPC error shape so the wire stays consistent.
func asProtocolErr(err error) error {
	if pe, ok := err.(*protocol.Error); ok {
		return pe
	}
	return protocol.ErrInternal(err.Error())
}
