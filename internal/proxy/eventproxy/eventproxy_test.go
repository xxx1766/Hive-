package eventproxy

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/anne-x/hive/internal/eventbus"
	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/rank"
	"github.com/anne-x/hive/internal/rpc"
	"github.com/anne-x/hive/internal/volume"
)

// fakeNotifier — duplicated locally because eventbus_test fakes are unexported.
type fakeNotifier struct {
	mu    sync.Mutex
	calls []rpc.EventsRecvParams
}

func (f *fakeNotifier) Notify(method string, params any) error {
	if method != rpc.MethodEventsRecv {
		return nil
	}
	p, ok := params.(rpc.EventsRecvParams)
	if !ok {
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

func newProxy(t *testing.T, name string, conn eventbus.Notifier, vols *volume.Manager, bus *eventbus.Manager) *Proxy {
	t.Helper()
	r := &rank.Rank{Name: "staff", MemoryAllowed: true}
	return &Proxy{
		RoomID:    "room-" + name,
		AgentName: name,
		Rank:      r,
		Volumes:   vols,
		Bus:       bus,
		Conn:      conn,
	}
}

func tempVolumes(t *testing.T) *volume.Manager {
	t.Helper()
	root := filepath.Join(t.TempDir(), "volumes")
	m, err := volume.New(root)
	if err != nil {
		t.Fatalf("volume.New: %v", err)
	}
	return m
}

func TestProxyGateBlocksWhenMemoryDisallowed(t *testing.T) {
	p := newProxy(t, "alice", &fakeNotifier{}, tempVolumes(t), eventbus.New(context.Background()))
	defer p.Bus.Shutdown()
	p.Rank = &rank.Rank{Name: "intern", MemoryAllowed: false}

	cases := []func() (any, error){
		func() (any, error) { return p.Publish(context.Background(), json.RawMessage(`{"scope":"","payload":null}`)) },
		func() (any, error) { return p.Subscribe(json.RawMessage(`{"scope":""}`)) },
		func() (any, error) { return p.Unsubscribe(json.RawMessage(`{"sub_id":"sub-x"}`)) },
	}
	for i, fn := range cases {
		_, err := fn()
		var pe *protocol.Error
		if !errors.As(err, &pe) || pe.Code != protocol.ErrCodePermissionDenied {
			t.Fatalf("case %d: want permission denied, got %v", i, err)
		}
	}
}

func TestProxySubscribeAndPublishSameRoom(t *testing.T) {
	bus := eventbus.New(context.Background())
	defer bus.Shutdown()
	vols := tempVolumes(t)

	notif := &fakeNotifier{}
	sub := newProxy(t, "alice", notif, vols, bus)
	pub := newProxy(t, "bob", &fakeNotifier{}, vols, bus)
	pub.RoomID = sub.RoomID // same room

	res, err := sub.Subscribe(json.RawMessage(`{"scope":""}`))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	subRes, ok := res.(rpc.EventsSubscribeResult)
	if !ok || subRes.SubID == "" {
		t.Fatalf("subscribe result: %+v", res)
	}

	if _, err := pub.Publish(context.Background(), json.RawMessage(`{"scope":"","payload":{"hi":1}}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got := notif.snapshot()
	if len(got) != 1 {
		t.Fatalf("subscriber got %d events, want 1", len(got))
	}
	ev := got[0]
	if ev.Scope != "" || ev.FromAgent != "bob" || ev.FromRoom != sub.RoomID {
		t.Fatalf("event params unexpected: %+v", ev)
	}
}

func TestProxyVolumeScopeRequiresVolume(t *testing.T) {
	bus := eventbus.New(context.Background())
	defer bus.Shutdown()
	vols := tempVolumes(t)
	p := newProxy(t, "alice", &fakeNotifier{}, vols, bus)

	// Subscribe to non-existent volume should error with the same hint as memproxy.
	_, err := p.Subscribe(json.RawMessage(`{"scope":"chatroom"}`))
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != protocol.ErrCodeInvalidParams {
		t.Fatalf("expected invalid params, got %v", err)
	}
	if !strings.Contains(pe.Message, "hive volume create chatroom") {
		t.Fatalf("error message lacks creation hint: %s", pe.Message)
	}

	// Same hint for publish.
	_, err = p.Publish(context.Background(), json.RawMessage(`{"scope":"chatroom","payload":null}`))
	if !errors.As(err, &pe) || pe.Code != protocol.ErrCodeInvalidParams {
		t.Fatalf("expected invalid params on publish, got %v", err)
	}

	// Once the volume exists, both calls succeed.
	if _, err := vols.Create("chatroom"); err != nil {
		t.Fatalf("create volume: %v", err)
	}
	if _, err := p.Subscribe(json.RawMessage(`{"scope":"chatroom"}`)); err != nil {
		t.Fatalf("subscribe(volume): %v", err)
	}
	if _, err := p.Publish(context.Background(), json.RawMessage(`{"scope":"chatroom","payload":null}`)); err != nil {
		t.Fatalf("publish(volume): %v", err)
	}
}

func TestProxyCrossRoomViaVolume(t *testing.T) {
	bus := eventbus.New(context.Background())
	defer bus.Shutdown()
	vols := tempVolumes(t)
	if _, err := vols.Create("chatroom"); err != nil {
		t.Fatalf("create volume: %v", err)
	}

	subNotif := &fakeNotifier{}
	subscriber := newProxy(t, "alice", subNotif, vols, bus)
	subscriber.RoomID = "room-A"

	publisher := newProxy(t, "bob", &fakeNotifier{}, vols, bus)
	publisher.RoomID = "room-B" // distinct Room

	if _, err := subscriber.Subscribe(json.RawMessage(`{"scope":"chatroom"}`)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := publisher.Publish(context.Background(), json.RawMessage(`{"scope":"chatroom","payload":"hello"}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}

	got := subNotif.snapshot()
	if len(got) != 1 {
		t.Fatalf("alice got %d events, want 1", len(got))
	}
	if got[0].Scope != "chatroom" || got[0].FromRoom != "room-B" || got[0].FromAgent != "bob" {
		t.Fatalf("unexpected event: %+v", got[0])
	}
}

func TestProxySameRoomScopeIsolatedFromOtherRoom(t *testing.T) {
	bus := eventbus.New(context.Background())
	defer bus.Shutdown()
	vols := tempVolumes(t)

	aN := &fakeNotifier{}
	a := newProxy(t, "alice", aN, vols, bus)
	a.RoomID = "room-A"

	b := newProxy(t, "bob", &fakeNotifier{}, vols, bus)
	b.RoomID = "room-B" // different room

	if _, err := a.Subscribe(json.RawMessage(`{"scope":""}`)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	// bob publishes with scope="" — that's b's *own* Room scope, NOT a's.
	if _, err := b.Publish(context.Background(), json.RawMessage(`{"scope":"","payload":null}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := aN.snapshot(); len(got) != 0 {
		t.Fatalf("alice should not receive events from another Room's scope=\"\", got %d", len(got))
	}
}

func TestProxyUnsubscribeOwnership(t *testing.T) {
	bus := eventbus.New(context.Background())
	defer bus.Shutdown()
	vols := tempVolumes(t)

	aliceNotif := &fakeNotifier{}
	alice := newProxy(t, "alice", aliceNotif, vols, bus)
	bob := newProxy(t, "bob", &fakeNotifier{}, vols, bus)
	bob.RoomID = alice.RoomID

	res, err := alice.Subscribe(json.RawMessage(`{"scope":""}`))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	subID := res.(rpc.EventsSubscribeResult).SubID

	// Bob can't cancel Alice's subscription even if he learns the sub_id.
	body, _ := json.Marshal(rpc.EventsUnsubscribeParams{SubID: subID})
	_, err = bob.Unsubscribe(body)
	var pe *protocol.Error
	if !errors.As(err, &pe) || pe.Code != protocol.ErrCodePermissionDenied {
		t.Fatalf("bob unsubscribing alice's sub: want permission denied, got %v", err)
	}

	// Alice can. Idempotent on second call.
	if _, err := alice.Unsubscribe(body); err != nil {
		t.Fatalf("alice unsubscribe: %v", err)
	}
	if _, err := alice.Unsubscribe(body); err != nil {
		t.Fatalf("alice second unsubscribe should be idempotent, got %v", err)
	}

	// Empty sub_id is rejected.
	_, err = alice.Unsubscribe(json.RawMessage(`{"sub_id":""}`))
	if !errors.As(err, &pe) || pe.Code != protocol.ErrCodeInvalidParams {
		t.Fatalf("empty sub_id: want invalid params, got %v", err)
	}
}

func TestProxyPublisherDoesNotEchoSelf(t *testing.T) {
	bus := eventbus.New(context.Background())
	defer bus.Shutdown()
	vols := tempVolumes(t)

	notif := &fakeNotifier{}
	p := newProxy(t, "alice", notif, vols, bus)
	if _, err := p.Subscribe(json.RawMessage(`{"scope":""}`)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if _, err := p.Publish(context.Background(), json.RawMessage(`{"scope":"","payload":null}`)); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if got := notif.snapshot(); len(got) != 0 {
		t.Fatalf("publisher echoed self: got %d events", len(got))
	}
}
