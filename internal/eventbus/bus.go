// Package eventbus is the daemon-wide pub/sub fabric that powers the
// Agent-facing events/* RPC family.
//
// Design (deliberately mirrors internal/router):
//
//   - One Bus per scope key (e.g. "room:abc-1", "volume:chatroom").
//   - Each Bus owns a single goroutine that drains a publish channel and
//     fans out events to current subscribers via Conn.Notify(events/recv, …).
//     Serialising delivery in one goroutine gives us deterministic per-scope
//     ordering and a single place to add quota/rate-limit hooks later.
//   - Subscribe / Unsubscribe touch a mutex-guarded map directly (same
//     shortcut Router takes for Register/Unregister) — they're rare and
//     don't need to flow through the actor.
//
// Backpressure: the publish channel is buffered (Manager.BufSize, default
// 256). When full, Publish returns an error rather than blocking, matching
// Router's contract. Per-subscriber Notify is synchronous (writer mutex on
// agent.Conn), so a slow subscriber slows the bus — same trade-off Router
// makes today.
package eventbus

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/anne-x/hive/internal/rpc"
)

// Notifier is the slice of *agent.Conn that the bus needs for delivery.
// Defining it as an interface in this package lets tests substitute a fake
// without pulling in the full agent process machinery.
type Notifier interface {
	Notify(method string, params any) error
}

// Subscriber identifies one subscription on a Bus. SubID is the token
// returned to the Agent and used by events/unsubscribe.
type Subscriber struct {
	SubID     string
	Notifier  Notifier
	AgentName string
	RoomID    string
}

// Envelope is what publishers put on the bus. Bus stamps Seq monotonically
// per-bus and wraps the rest into rpc.EventsRecvParams for delivery.
type Envelope struct {
	Scope     string // wire scope to echo back ("" or volume name)
	FromRoom  string
	FromAgent string
	Payload   json.RawMessage
}

// Bus is a single-scope event broker. Construct via Manager.GetOrCreate.
type Bus struct {
	key string

	mu   sync.RWMutex
	subs map[string]Subscriber // sub_id → subscription

	pub  chan publishOp
	done chan struct{}
	once sync.Once
	seq  atomic.Uint64
}

type publishOp struct {
	env   Envelope
	reply chan error
}

func newBus(key string, bufSize int) *Bus {
	return &Bus{
		key:  key,
		subs: make(map[string]Subscriber),
		pub:  make(chan publishOp, bufSize),
		done: make(chan struct{}),
	}
}

// Key is the scope identifier used by the Manager (for tests / debug).
func (b *Bus) Key() string { return b.key }

// Subscribe attaches sub to the bus. Idempotent on SubID — re-subscribing
// the same SubID overwrites the previous Subscriber record.
func (b *Bus) Subscribe(sub Subscriber) {
	b.mu.Lock()
	b.subs[sub.SubID] = sub
	b.mu.Unlock()
}

// Unsubscribe removes a subscription by SubID without an owner check.
// Returns true if it existed. Callers exposed to untrusted Agents should
// prefer UnsubscribeOwned to prevent one Agent from cancelling another's
// subscription if they ever observe its sub_id.
func (b *Bus) Unsubscribe(subID string) bool {
	b.mu.Lock()
	_, ok := b.subs[subID]
	delete(b.subs, subID)
	b.mu.Unlock()
	return ok
}

// UnsubscribeOwned removes a subscription only if owner matches the
// Notifier registered with that SubID. Returns (found, owned):
//
//	found=false               — sub_id doesn't exist
//	found=true,  owned=false  — exists but owned by a different Notifier
//	found=true,  owned=true   — removed
func (b *Bus) UnsubscribeOwned(subID string, owner Notifier) (found, owned bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.subs[subID]
	if !ok {
		return false, false
	}
	if s.Notifier != owner {
		return true, false
	}
	delete(b.subs, subID)
	return true, true
}

// UnsubscribeNotifier removes every subscription whose Notifier matches.
// Used when an Agent exits so subscriptions don't leak.
func (b *Bus) UnsubscribeNotifier(n Notifier) {
	b.mu.Lock()
	for id, s := range b.subs {
		if s.Notifier == n {
			delete(b.subs, id)
		}
	}
	b.mu.Unlock()
}

// Subscribers returns a snapshot of current subscriber count (for tests).
func (b *Bus) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}

// Publish enqueues an envelope for delivery and blocks until the actor has
// attempted to deliver it (so the caller's events/publish RPC can surface
// any wire-level error from a slow subscriber, mirroring router.Router.Send).
func (b *Bus) Publish(ctx context.Context, env Envelope) error {
	reply := make(chan error, 1)
	select {
	case b.pub <- publishOp{env: env, reply: reply}:
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return fmt.Errorf("eventbus: bus stopped")
	}
	select {
	case err := <-reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-b.done:
		return fmt.Errorf("eventbus: bus stopped")
	}
}

// Run owns the single delivery goroutine. Returns when ctx is cancelled
// or Stop is called.
func (b *Bus) Run(ctx context.Context) {
	defer b.once.Do(func() { close(b.done) })
	for {
		select {
		case op := <-b.pub:
			b.deliver(op)
		case <-ctx.Done():
			return
		}
	}
}

// Stop ends the actor. Pending Publish calls return "bus stopped".
func (b *Bus) Stop() { b.once.Do(func() { close(b.done) }) }

func (b *Bus) deliver(op publishOp) {
	seq := b.seq.Add(1)

	// Snapshot the subscriber list so Notify can run with the lock released
	// (Notify holds the Conn's writer mutex, which we don't want to nest
	// under the bus's lock).
	b.mu.RLock()
	targets := make([]Subscriber, 0, len(b.subs))
	for _, s := range b.subs {
		// Skip the publisher's own subscriptions on the same bus —
		// publishing should never echo back to self.
		if s.RoomID == op.env.FromRoom && s.AgentName == op.env.FromAgent {
			continue
		}
		targets = append(targets, s)
	}
	b.mu.RUnlock()

	var firstErr error
	for _, t := range targets {
		params := rpc.EventsRecvParams{
			Scope:     op.env.Scope,
			SubID:     t.SubID,
			FromRoom:  op.env.FromRoom,
			FromAgent: op.env.FromAgent,
			Seq:       seq,
			Payload:   op.env.Payload,
		}
		if err := t.Notifier.Notify(rpc.MethodEventsRecv, params); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	op.reply <- firstErr
}
