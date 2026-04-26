package eventbus

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anne-x/hive/internal/rpc"
)

// fakeNotifier records Notify calls for assertions.
type fakeNotifier struct {
	mu     sync.Mutex
	calls  []rpc.EventsRecvParams
	failOn atomic.Int32 // 1 ⇒ next Notify returns error
	errVal error
}

func (f *fakeNotifier) Notify(method string, params any) error {
	if f.failOn.Load() == 1 {
		f.failOn.Store(0)
		return f.errVal
	}
	if method != rpc.MethodEventsRecv {
		return errors.New("unexpected method: " + method)
	}
	p, ok := params.(rpc.EventsRecvParams)
	if !ok {
		// Allow JSON-encoded form for completeness.
		raw, _ := json.Marshal(params)
		var dec rpc.EventsRecvParams
		_ = json.Unmarshal(raw, &dec)
		p = dec
	}
	f.mu.Lock()
	f.calls = append(f.calls, p)
	f.mu.Unlock()
	return nil
}

func (f *fakeNotifier) snapshot() []rpc.EventsRecvParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]rpc.EventsRecvParams, len(f.calls))
	copy(out, f.calls)
	return out
}

func startBus(t *testing.T) (*Bus, context.CancelFunc) {
	t.Helper()
	b := newBus("test:scope", DefaultBufSize)
	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)
	t.Cleanup(func() {
		cancel()
		b.Stop()
	})
	return b, cancel
}

func TestBusDeliversToAllSubscribers(t *testing.T) {
	b, _ := startBus(t)

	a := &fakeNotifier{}
	c := &fakeNotifier{}
	b.Subscribe(Subscriber{SubID: "sub-a", Notifier: a, AgentName: "alice", RoomID: "room-1"})
	b.Subscribe(Subscriber{SubID: "sub-c", Notifier: c, AgentName: "carol", RoomID: "room-2"})

	if err := b.Publish(context.Background(), Envelope{
		Scope:     "chatroom",
		FromRoom:  "room-99",
		FromAgent: "publisher",
		Payload:   json.RawMessage(`{"hi":1}`),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got := a.snapshot()
	if len(got) != 1 {
		t.Fatalf("alice got %d events, want 1", len(got))
	}
	ev := got[0]
	if ev.SubID != "sub-a" || ev.Scope != "chatroom" || ev.FromRoom != "room-99" || ev.FromAgent != "publisher" {
		t.Fatalf("unexpected event params: %+v", ev)
	}
	if ev.Seq != 1 {
		t.Fatalf("seq = %d, want 1", ev.Seq)
	}
	if string(ev.Payload) != `{"hi":1}` {
		t.Fatalf("payload = %s", ev.Payload)
	}

	if got := c.snapshot(); len(got) != 1 || got[0].SubID != "sub-c" {
		t.Fatalf("carol unexpected events: %+v", got)
	}
}

func TestBusSkipsPublisherSelf(t *testing.T) {
	b, _ := startBus(t)

	pub := &fakeNotifier{}
	other := &fakeNotifier{}
	b.Subscribe(Subscriber{SubID: "self", Notifier: pub, AgentName: "alice", RoomID: "room-1"})
	b.Subscribe(Subscriber{SubID: "other", Notifier: other, AgentName: "bob", RoomID: "room-1"})

	if err := b.Publish(context.Background(), Envelope{
		FromRoom: "room-1", FromAgent: "alice", Payload: json.RawMessage(`null`),
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if got := pub.snapshot(); len(got) != 0 {
		t.Fatalf("publisher should not see own events, got %d", len(got))
	}
	if got := other.snapshot(); len(got) != 1 {
		t.Fatalf("other should receive 1 event, got %d", len(got))
	}
}

func TestBusSeqMonotonic(t *testing.T) {
	b, _ := startBus(t)
	n := &fakeNotifier{}
	b.Subscribe(Subscriber{SubID: "s", Notifier: n, AgentName: "a", RoomID: "r"})

	for i := 0; i < 5; i++ {
		if err := b.Publish(context.Background(), Envelope{
			FromRoom: "src", FromAgent: "p", Payload: json.RawMessage(`null`),
		}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	got := n.snapshot()
	if len(got) != 5 {
		t.Fatalf("got %d events, want 5", len(got))
	}
	for i, ev := range got {
		want := uint64(i + 1)
		if ev.Seq != want {
			t.Fatalf("event %d seq = %d, want %d", i, ev.Seq, want)
		}
	}
}

func TestBusUnsubscribe(t *testing.T) {
	b, _ := startBus(t)
	n := &fakeNotifier{}
	b.Subscribe(Subscriber{SubID: "s1", Notifier: n, AgentName: "a", RoomID: "r"})
	if !b.Unsubscribe("s1") {
		t.Fatalf("Unsubscribe of existing sub returned false")
	}
	if b.Unsubscribe("s1") {
		t.Fatalf("Unsubscribe of missing sub returned true (not idempotent)")
	}
	if err := b.Publish(context.Background(), Envelope{Payload: json.RawMessage(`null`)}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := n.snapshot(); len(got) != 0 {
		t.Fatalf("expected no delivery after unsubscribe, got %d", len(got))
	}
}

func TestBusUnsubscribeNotifier(t *testing.T) {
	b, _ := startBus(t)
	a := &fakeNotifier{}
	c := &fakeNotifier{}
	b.Subscribe(Subscriber{SubID: "s-a1", Notifier: a, AgentName: "alice", RoomID: "r"})
	b.Subscribe(Subscriber{SubID: "s-a2", Notifier: a, AgentName: "alice", RoomID: "r"})
	b.Subscribe(Subscriber{SubID: "s-c", Notifier: c, AgentName: "carol", RoomID: "r"})

	b.UnsubscribeNotifier(a)

	if b.Subscribers() != 1 {
		t.Fatalf("subscribers after UnsubscribeNotifier = %d, want 1", b.Subscribers())
	}

	if err := b.Publish(context.Background(), Envelope{Payload: json.RawMessage(`null`)}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if len(a.snapshot()) != 0 {
		t.Fatalf("alice received events after UnsubscribeNotifier")
	}
	if len(c.snapshot()) != 1 {
		t.Fatalf("carol should still receive events")
	}
}

func TestBusPublishStoppedReturnsError(t *testing.T) {
	b := newBus("k", DefaultBufSize)
	ctx, cancel := context.WithCancel(context.Background())
	go b.Run(ctx)
	cancel()
	b.Stop()
	// Give the goroutine a beat to wind down.
	time.Sleep(10 * time.Millisecond)

	err := b.Publish(context.Background(), Envelope{Payload: json.RawMessage(`null`)})
	if err == nil {
		t.Fatalf("expected error from publish on stopped bus")
	}
}

func TestBusPublishCancelledContext(t *testing.T) {
	b := newBus("k", 1) // tiny buffer
	// Don't start the actor — pub channel will never drain.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := b.Publish(ctx, Envelope{Payload: json.RawMessage(`null`)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
