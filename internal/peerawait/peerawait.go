// Package peerawait routes inbound peer messages to either a registered
// awaiter (a peer_call tool currently blocked waiting for a reply) or a
// fallback channel that the runner's main goroutine drives via
// runFromPeer. A runner spawns one peer-router goroutine that consumes
// a.Peers() and calls Dispatch on every message; tool implementations
// call Register before sending and read the returned channel until the
// reply arrives.
//
// Routing key is (from, convID) — that's the natural tuple for a
// request/response peer_call: "I'm coordinator, I sent paper-writer a
// task tagged with conv-X, I want the reply tagged the same way." If
// multiple awaiters exist for the same key (rare; would mean overlapping
// peer_calls to the same agent in the same conversation) only the first
// one wins; subsequent calls receive a fresh channel for the next reply.
//
// Both hive-skill-runner and hive-workflow-runner consume this package
// so the awaiter behaviour stays consistent across runner kinds.
package peerawait

import (
	"sync"

	hive "github.com/anne-x/hive/sdk/go"
)

// Awaiter is the awaiter registry + fallback fan-out used by a runner's
// peer-router goroutine.
type Awaiter struct {
	mu       sync.Mutex
	waiters  map[string]chan *hive.PeerMessage
	fallback chan *hive.PeerMessage
}

// awaitBuffer is the per-awaiter channel depth. 1 is enough — a peer_call
// only reads a single reply before unregistering.
const awaitBuffer = 1

// fallbackBuffer is the depth of the fallback channel used by the
// runner's main goroutine. Sized so a burst of inbound peers doesn't
// stall the peer-router while the main goroutine is busy in a long
// runOne / runFromPeer cycle. 16 mirrors the SDK's own peers channel
// capacity (see sdk/go/agent.go).
const fallbackBuffer = 16

// New constructs an empty Awaiter ready to be wired into a runner.
func New() *Awaiter {
	return &Awaiter{
		waiters:  map[string]chan *hive.PeerMessage{},
		fallback: make(chan *hive.PeerMessage, fallbackBuffer),
	}
}

func awaitKey(from, convID string) string {
	return from + "|" + convID
}

// Register reserves an awaiter slot for (from, convID) and returns the
// channel the reply will arrive on plus an unregister func. The
// caller must call the unregister func (typically via defer) so a
// timeout / context-cancel doesn't leak the entry.
//
// If a slot for the same key already exists, Register overwrites it —
// the old waiter's channel is closed (so its select-loop wakes with a
// zero value and treats it as cancellation). This is a defensive
// behaviour for the unusual case of overlapping peer_calls to the same
// peer in the same conversation; well-behaved skills don't do this.
func (a *Awaiter) Register(from, convID string) (<-chan *hive.PeerMessage, func()) {
	key := awaitKey(from, convID)
	ch := make(chan *hive.PeerMessage, awaitBuffer)
	a.mu.Lock()
	if old, ok := a.waiters[key]; ok {
		close(old)
	}
	a.waiters[key] = ch
	a.mu.Unlock()
	cancel := func() {
		a.mu.Lock()
		if cur, ok := a.waiters[key]; ok && cur == ch {
			delete(a.waiters, key)
		}
		a.mu.Unlock()
	}
	return ch, cancel
}

// Dispatch routes p to a registered awaiter if one matches; otherwise
// hands it off to the fallback channel for the runner's main goroutine
// to process via runFromPeer. Returns true when delivered to an
// awaiter, false when delivered (or attempted) to the fallback.
//
// Fallback delivery is non-blocking: if the fallback channel is full
// (main goroutine is slow), the peer is dropped and a count would be
// incremented in a richer implementation. For the demo we accept the
// drop — skills that depend on peer-driven progress should use
// peer_call (which routes through a dedicated awaiter, never the
// fallback).
func (a *Awaiter) Dispatch(p *hive.PeerMessage) bool {
	if p == nil {
		return false
	}
	key := awaitKey(p.From, p.ConvID)
	a.mu.Lock()
	ch, ok := a.waiters[key]
	if ok {
		delete(a.waiters, key)
	}
	a.mu.Unlock()
	if ok {
		// Non-blocking send: awaitBuffer = 1, channel is fresh, send
		// always succeeds. Defensive select handles the closed-by-
		// race-with-cancel case (see Register's overwrite path).
		select {
		case ch <- p:
		default:
		}
		return true
	}
	// No awaiter — try fallback.
	select {
	case a.fallback <- p:
	default:
		// Fallback full — drop. Acceptable for the demo; see comment above.
	}
	return false
}

// Fallback returns the channel of peer messages NOT consumed by any
// awaiter. The runner's main goroutine ranges over this so runFromPeer
// keeps working for ad-hoc inbound messages.
func (a *Awaiter) Fallback() <-chan *hive.PeerMessage {
	return a.fallback
}

// Close shuts the fallback channel so the main goroutine's range / select
// terminates. Idempotent.
func (a *Awaiter) Close() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, ch := range a.waiters {
		close(ch)
		delete(a.waiters, k)
	}
	// fallback close is one-shot; guard with recover so multiple Close
	// calls don't panic.
	defer func() { _ = recover() }()
	close(a.fallback)
}
