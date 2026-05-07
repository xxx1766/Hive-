// Package quota is a channel-based actor for (Room, Agent) quota accounting.
//
// Why an actor and not a sync.Map? Two reasons:
//
//  1. Atomic check-and-decrement. A token budget has to be "reserve N,
//     proceed if possible, else reject" — two ops under the same lock or,
//     more Go-idiomatically, serialized through one goroutine.
//
//  2. One clean place to emit audit events when quota is reached.
//
// The actor's own state is only touched by its Run goroutine, so no locks.
// Callers interact via Consume / SetLimit / Remaining; each builds a request
// and waits for the actor's reply on a per-call channel.
package quota

import (
	"context"
	"fmt"
	"sync"
)

// Key uniquely identifies a counter bucket.
type Key struct {
	RoomID   string
	Agent    string
	Resource string // e.g. "tokens:openai:gpt-4o-mini" or "http"
}

// Result is the outcome of a Consume call.
type Result struct {
	Allowed   bool // false ⇒ quota exceeded (would have gone negative)
	Remaining int  // after the attempted consume (unchanged on reject)
	Unlimited bool // no limit configured for this Key
}

type opType int

const (
	opConsume opType = iota
	opSetLimit
	opRemaining
	opReset
	// opUncharge atomically decrements consumed (clamped ≥ 0) without
	// touching the limit. Used by hire/junior refund-on-exit: when a
	// subordinate Agent exits with unused budget, the daemon refunds
	// the parent's bucket by uncharging the unused amount it consumed
	// at hire time. Pure inverse of opConsume.
	opUncharge
)

type request struct {
	op     opType
	key    Key
	amount int
	reply  chan Result
}

// Actor serializes all quota state mutations through one goroutine.
type Actor struct {
	reqs chan request
	done chan struct{}
	once sync.Once
}

// New returns a ready-to-Run Actor. Buffer size affects how many concurrent
// proxy handlers can enqueue before blocking; 1024 is generous for demo.
func New(bufSize int) *Actor {
	if bufSize <= 0 {
		bufSize = 1024
	}
	return &Actor{
		reqs: make(chan request, bufSize),
		done: make(chan struct{}),
	}
}

// Run owns the mutable state and services requests until ctx is cancelled.
// Safe to call once; re-running after Stop is not supported.
func (a *Actor) Run(ctx context.Context) {
	defer a.once.Do(func() { close(a.done) })

	limits := make(map[Key]int)     // absolute max; absence = unlimited
	consumed := make(map[Key]int)   // how much already used

	for {
		select {
		case r := <-a.reqs:
			switch r.op {
			case opConsume:
				lim, hasLim := limits[r.key]
				if !hasLim {
					consumed[r.key] += r.amount
					r.reply <- Result{Allowed: true, Unlimited: true, Remaining: -1}
					continue
				}
				used := consumed[r.key]
				if used+r.amount > lim {
					r.reply <- Result{Allowed: false, Remaining: lim - used}
					continue
				}
				consumed[r.key] = used + r.amount
				r.reply <- Result{Allowed: true, Remaining: lim - (used + r.amount)}

			case opSetLimit:
				limits[r.key] = r.amount
				r.reply <- Result{Allowed: true, Remaining: r.amount - consumed[r.key]}

			case opRemaining:
				if lim, ok := limits[r.key]; ok {
					r.reply <- Result{Remaining: lim - consumed[r.key], Allowed: true}
				} else {
					r.reply <- Result{Unlimited: true, Remaining: -1, Allowed: true}
				}

			case opReset:
				delete(consumed, r.key)
				r.reply <- Result{Allowed: true}

			case opUncharge:
				// Inverse of opConsume — clamped at zero. We never
				// decrement past zero because that would mean the
				// child consumed less than nothing, which is nonsense.
				cur := consumed[r.key]
				cur -= r.amount
				if cur < 0 {
					cur = 0
				}
				consumed[r.key] = cur
				if lim, ok := limits[r.key]; ok {
					r.reply <- Result{Allowed: true, Remaining: lim - cur}
				} else {
					r.reply <- Result{Allowed: true, Unlimited: true, Remaining: -1}
				}
			}

		case <-ctx.Done():
			return
		}
	}
}

// Stop terminates the Actor. Queued requests may be dropped.
func (a *Actor) Stop() { a.once.Do(func() { close(a.done) }) }

// Consume attempts to charge `amount` against the key. amount must be >= 0.
func (a *Actor) Consume(ctx context.Context, key Key, amount int) (Result, error) {
	if amount < 0 {
		return Result{}, fmt.Errorf("negative consume: %d", amount)
	}
	return a.call(ctx, request{op: opConsume, key: key, amount: amount})
}

// SetLimit installs an absolute max for the key.
func (a *Actor) SetLimit(ctx context.Context, key Key, limit int) (Result, error) {
	return a.call(ctx, request{op: opSetLimit, key: key, amount: limit})
}

// Remaining reports how much of the limit is unused. -1 + Unlimited=true
// means no limit is set.
func (a *Actor) Remaining(ctx context.Context, key Key) (Result, error) {
	return a.call(ctx, request{op: opRemaining, key: key})
}

// Reset zeros the consumed counter for the key (limit untouched).
func (a *Actor) Reset(ctx context.Context, key Key) (Result, error) {
	return a.call(ctx, request{op: opReset, key: key})
}

// Uncharge atomically decrements consumed by `amount` (clamped at 0).
// Used by the daemon to refund unused subordinate quota back to the
// parent's bucket when a subordinate Agent exits — see
// hire_junior carve semantics in ARCHITECTURE.md § "Auto-hire 与
// 配额 carve". amount must be >= 0.
func (a *Actor) Uncharge(ctx context.Context, key Key, amount int) (Result, error) {
	if amount < 0 {
		return Result{}, fmt.Errorf("negative uncharge: %d", amount)
	}
	return a.call(ctx, request{op: opUncharge, key: key, amount: amount})
}

func (a *Actor) call(ctx context.Context, req request) (Result, error) {
	req.reply = make(chan Result, 1)
	select {
	case a.reqs <- req:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-a.done:
		return Result{}, fmt.Errorf("quota actor stopped")
	}
	select {
	case res := <-req.reply:
		return res, nil
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-a.done:
		return Result{}, fmt.Errorf("quota actor stopped")
	}
}
