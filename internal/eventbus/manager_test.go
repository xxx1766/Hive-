package eventbus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestManagerKeyHelpers(t *testing.T) {
	if got := RoomKey("abc"); got != "room:abc" {
		t.Fatalf("RoomKey = %q", got)
	}
	if got := VolumeKey("chatroom"); got != "volume:chatroom" {
		t.Fatalf("VolumeKey = %q", got)
	}
}

func TestManagerGetOrCreateReusesBus(t *testing.T) {
	m := New(context.Background())
	defer m.Shutdown()

	a := m.GetOrCreate("scope-1")
	b := m.GetOrCreate("scope-1")
	if a != b {
		t.Fatalf("GetOrCreate returned a fresh bus on second call")
	}
	if got := m.Get("scope-1"); got != a {
		t.Fatalf("Get(existing) returned different bus")
	}
	if got := m.Get("nope"); got != nil {
		t.Fatalf("Get(missing) returned %v, want nil", got)
	}
}

func TestManagerNewSubIDUnique(t *testing.T) {
	m := New(context.Background())
	defer m.Shutdown()
	seen := make(map[string]struct{})
	for i := 0; i < 100; i++ {
		id := m.NewSubID()
		if !strings.HasPrefix(id, "sub-") {
			t.Fatalf("sub id %q missing prefix", id)
		}
		// 4 (prefix "sub-") + 32 hex chars = 36 total
		if len(id) != 4+32 {
			t.Fatalf("sub id %q wrong length %d", id, len(id))
		}
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate sub id: %s", id)
		}
		seen[id] = struct{}{}
	}
}

func TestManagerUnsubscribeOwnedCrossesBuses(t *testing.T) {
	m := New(context.Background())
	defer m.Shutdown()
	b1 := m.GetOrCreate("k1")
	b2 := m.GetOrCreate("k2")
	n := &fakeNotifier{}
	b1.Subscribe(Subscriber{SubID: "x", Notifier: n})
	b2.Subscribe(Subscriber{SubID: "y", Notifier: n})

	found, owned := m.UnsubscribeOwned("y", n)
	if !found || !owned {
		t.Fatalf("UnsubscribeOwned(y,n): found=%v owned=%v", found, owned)
	}
	if b2.Subscribers() != 0 {
		t.Fatalf("b2 still has %d subscribers", b2.Subscribers())
	}
	if b1.Subscribers() != 1 {
		t.Fatalf("b1 lost a subscriber it shouldn't have")
	}
	if found, _ = m.UnsubscribeOwned("nonexistent", n); found {
		t.Fatalf("UnsubscribeOwned of unknown id returned found=true")
	}

	// Different owner can't cancel an existing subscription.
	other := &fakeNotifier{}
	found, owned = m.UnsubscribeOwned("x", other)
	if !found {
		t.Fatalf("UnsubscribeOwned(x, other): expected found=true")
	}
	if owned {
		t.Fatalf("UnsubscribeOwned(x, other): expected owned=false (different owner)")
	}
	if b1.Subscribers() != 1 {
		t.Fatalf("non-owner managed to remove subscription")
	}
}

func TestManagerUnsubscribeNotifierClearsAll(t *testing.T) {
	m := New(context.Background())
	defer m.Shutdown()
	b1 := m.GetOrCreate("k1")
	b2 := m.GetOrCreate("k2")
	a := &fakeNotifier{}
	c := &fakeNotifier{}
	b1.Subscribe(Subscriber{SubID: "a1", Notifier: a})
	b2.Subscribe(Subscriber{SubID: "a2", Notifier: a})
	b2.Subscribe(Subscriber{SubID: "c1", Notifier: c})

	m.UnsubscribeNotifier(a)

	if b1.Subscribers() != 0 || b2.Subscribers() != 1 {
		t.Fatalf("unexpected subscriber state after UnsubscribeNotifier: b1=%d b2=%d",
			b1.Subscribers(), b2.Subscribers())
	}
}

func TestManagerRemoveScopeStopsBus(t *testing.T) {
	m := New(context.Background())
	defer m.Shutdown()
	b := m.GetOrCreate("k")
	n := &fakeNotifier{}
	b.Subscribe(Subscriber{SubID: "s", Notifier: n, AgentName: "a", RoomID: "r"})

	m.RemoveScope("k")

	// New GetOrCreate returns a fresh bus (different pointer).
	b2 := m.GetOrCreate("k")
	if b == b2 {
		t.Fatalf("RemoveScope didn't replace the bus")
	}
	if b2.Subscribers() != 0 {
		t.Fatalf("fresh bus has stale subscribers")
	}

	// Publishing on the stopped bus must error.
	err := b.Publish(context.Background(), Envelope{Payload: json.RawMessage(`null`)})
	if err == nil {
		t.Fatalf("publish on stopped bus should error")
	}
}

func TestManagerShutdownStopsAllBuses(t *testing.T) {
	m := New(context.Background())
	b := m.GetOrCreate("k")
	m.Shutdown()
	err := b.Publish(context.Background(), Envelope{Payload: json.RawMessage(`null`)})
	if err == nil {
		t.Fatalf("publish on bus after Shutdown should error")
	}
}
