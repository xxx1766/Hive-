package quota

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func setupActor(t *testing.T) (*Actor, context.CancelFunc) {
	t.Helper()
	a := New(0)
	ctx, cancel := context.WithCancel(context.Background())
	go a.Run(ctx)
	return a, cancel
}

func TestConsumeBelowLimit(t *testing.T) {
	a, cancel := setupActor(t)
	defer cancel()

	ctx := context.Background()
	k := Key{RoomID: "r1", Agent: "fetch", Resource: "http"}
	if _, err := a.SetLimit(ctx, k, 5); err != nil {
		t.Fatalf("SetLimit: %v", err)
	}

	for i := 1; i <= 5; i++ {
		res, err := a.Consume(ctx, k, 1)
		if err != nil {
			t.Fatalf("Consume %d: %v", i, err)
		}
		if !res.Allowed {
			t.Fatalf("Consume %d should be allowed, got %+v", i, res)
		}
		if res.Remaining != 5-i {
			t.Fatalf("Consume %d remaining: got %d want %d", i, res.Remaining, 5-i)
		}
	}
}

func TestConsumeOverLimit(t *testing.T) {
	a, cancel := setupActor(t)
	defer cancel()
	ctx := context.Background()
	k := Key{RoomID: "r1", Agent: "fetch", Resource: "http"}
	a.SetLimit(ctx, k, 3)

	for i := 0; i < 3; i++ {
		r, _ := a.Consume(ctx, k, 1)
		if !r.Allowed {
			t.Fatal("expected allowed")
		}
	}
	r, _ := a.Consume(ctx, k, 1)
	if r.Allowed {
		t.Fatal("4th should be rejected")
	}
	if r.Remaining != 0 {
		t.Fatalf("rejected remaining should be 0, got %d", r.Remaining)
	}
	// Subsequent allowed=false attempts must not further decrement.
	r, _ = a.Consume(ctx, k, 1)
	if r.Allowed {
		t.Fatal("5th also rejected")
	}
	if r.Remaining != 0 {
		t.Fatalf("remaining stayed at 0, got %d", r.Remaining)
	}
}

func TestConsumeUnlimited(t *testing.T) {
	a, cancel := setupActor(t)
	defer cancel()
	ctx := context.Background()
	k := Key{RoomID: "r1", Agent: "a", Resource: "uncapped"}

	res, _ := a.Consume(ctx, k, 1000000)
	if !res.Allowed || !res.Unlimited {
		t.Fatalf("unlimited expected, got %+v", res)
	}
}

func TestIsolationBetweenKeys(t *testing.T) {
	// (Room A, Agent, http) and (Room B, Agent, http) must be independent.
	a, cancel := setupActor(t)
	defer cancel()
	ctx := context.Background()

	kA := Key{RoomID: "A", Agent: "fetch", Resource: "http"}
	kB := Key{RoomID: "B", Agent: "fetch", Resource: "http"}
	a.SetLimit(ctx, kA, 5)
	a.SetLimit(ctx, kB, 5)

	for i := 0; i < 5; i++ {
		if r, _ := a.Consume(ctx, kA, 1); !r.Allowed {
			t.Fatalf("A call %d rejected", i)
		}
	}
	rA, _ := a.Consume(ctx, kA, 1)
	if rA.Allowed {
		t.Fatal("A should be exhausted")
	}
	// B should still be untouched.
	for i := 0; i < 5; i++ {
		if r, _ := a.Consume(ctx, kB, 1); !r.Allowed {
			t.Fatalf("B unaffected but call %d rejected: %+v", i, r)
		}
	}
}

func TestConcurrentConsume(t *testing.T) {
	// Many goroutines racing to consume from one key.
	// The actor must hand out exactly `limit` "allowed" responses, no more.
	a, cancel := setupActor(t)
	defer cancel()
	ctx := context.Background()
	k := Key{RoomID: "r", Agent: "a", Resource: "http"}
	const limit = 100
	a.SetLimit(ctx, k, limit)

	const concurrency = 50
	const perG = 10
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < concurrency; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				if r, _ := a.Consume(ctx, k, 1); r.Allowed {
					allowed.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if int(allowed.Load()) != limit {
		t.Fatalf("allowed total: got %d want %d", allowed.Load(), limit)
	}
}

func TestContextCancelWhileEnqueued(t *testing.T) {
	a, cancel := setupActor(t)
	defer cancel()
	ctx, ctxCancel := context.WithCancel(context.Background())
	k := Key{RoomID: "r", Agent: "a", Resource: "x"}

	ctxCancel() // cancel before the call even enqueues
	_, err := a.Consume(ctx, k, 1)
	if err == nil {
		t.Fatalf("expected cancel error")
	}
	if !isCancelErr(err) {
		t.Fatalf("expected context cancel, got %v", err)
	}
}

func isCancelErr(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}

func TestActorStops(t *testing.T) {
	a := New(0)
	ctx, cancel := context.WithCancel(context.Background())
	go a.Run(ctx)
	cancel()
	time.Sleep(10 * time.Millisecond)
	// Further calls should return "stopped" error quickly (context cancelled).
	_, err := a.Consume(context.Background(), Key{Resource: "x"}, 1)
	if err == nil {
		t.Fatal("expected error after stop")
	}
}
