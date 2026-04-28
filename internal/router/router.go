// Package router is a per-Room peer-to-peer message bus.
//
// Design note (channel-based actor):
//
// All peer/send requests from Agents in a Room funnel into a single
// `routes` channel. One goroutine drains this channel, performs Rank
// checks, looks up the target Agent's Conn, and delivers a peer/recv
// notification. Serializing here gives us:
//
//   - deterministic per-Room ordering for auditing
//   - a single place to add quota/rate-limit hooks
//   - natural back-pressure if a peer's Conn is slow
//
// The Router does NOT know about Rank — it takes an AuthFn injected by the
// Room so rank/quota decisions live in their own package.
package router

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/anne-x/hive/internal/agent"
	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/rpc"
)

// AuthFn decides whether `from` may send to `to` in this Room.
// If the call is denied it returns a *protocol.Error which is surfaced to
// the sender as the peer/send response.
type AuthFn func(from, to string) error

// Router fans peer/send calls among Agents in a single Room.
type Router struct {
	roomID string
	auth   AuthFn

	mu     sync.RWMutex
	agents map[string]*agent.Conn

	routes chan envelope
	done   chan struct{}
	once   sync.Once
}

type envelope struct {
	from    string
	to      string
	convID  string // non-empty = part of a Conversation; propagated to peer/recv
	payload json.RawMessage
	reply   chan error
}

// New builds a Router. Call Register for each Agent, then Run.
// routesBuffer controls channel depth — bigger reduces blocking on peers
// that are momentarily slow; 256 is plenty for demo.
func New(roomID string, auth AuthFn, routesBuffer int) *Router {
	if routesBuffer <= 0 {
		routesBuffer = 256
	}
	if auth == nil {
		auth = func(string, string) error { return nil }
	}
	return &Router{
		roomID: roomID,
		auth:   auth,
		agents: make(map[string]*agent.Conn),
		routes: make(chan envelope, routesBuffer),
		done:   make(chan struct{}),
	}
}

// Register attaches an Agent under its image name. Safe to call while Run is active.
func (r *Router) Register(imageName string, conn *agent.Conn) {
	r.mu.Lock()
	r.agents[imageName] = conn
	r.mu.Unlock()
}

// Unregister detaches. Pending envelopes to this Agent will fail.
func (r *Router) Unregister(imageName string) {
	r.mu.Lock()
	delete(r.agents, imageName)
	r.mu.Unlock()
}

// Send enqueues a message and blocks until the routing goroutine has
// attempted delivery (returning auth or lookup errors synchronously is
// what makes the Agent's peer/send RPC behave sanely). convID is
// propagated to peer/recv on the receiving side; pass "" for ad-hoc
// peer messages outside any Conversation.
func (r *Router) Send(ctx context.Context, from, to, convID string, payload json.RawMessage) error {
	reply := make(chan error, 1)
	select {
	case r.routes <- envelope{from: from, to: to, convID: convID, payload: payload, reply: reply}:
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return fmt.Errorf("router stopped")
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-r.done:
		return fmt.Errorf("router stopped")
	}
}

// Run owns the single routing goroutine. Returns when ctx is cancelled.
func (r *Router) Run(ctx context.Context) {
	defer r.once.Do(func() { close(r.done) })
	for {
		select {
		case env := <-r.routes:
			r.handle(ctx, env)
		case <-ctx.Done():
			return
		}
	}
}

// Stop cancels all pending and future sends.
func (r *Router) Stop() { r.once.Do(func() { close(r.done) }) }

func (r *Router) handle(ctx context.Context, env envelope) {
	if err := r.auth(env.from, env.to); err != nil {
		env.reply <- err
		return
	}
	r.mu.RLock()
	target, ok := r.agents[env.to]
	r.mu.RUnlock()
	if !ok {
		env.reply <- &protocol.Error{
			Code:    protocol.ErrCodePeerNotFound,
			Message: fmt.Sprintf("peer not found in room: %s", env.to),
		}
		return
	}
	err := target.Notify(rpc.MethodPeerRecv, rpc.PeerRecvParams{
		From:    env.from,
		Payload: env.payload,
		ConvID:  env.convID,
	})
	env.reply <- err
}
