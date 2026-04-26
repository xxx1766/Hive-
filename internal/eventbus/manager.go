package eventbus

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sync"
)

// DefaultBufSize is the per-Bus publish-channel depth.
const DefaultBufSize = 256

// Scope key prefixes. Kept here so the proxy and the daemon can build the
// same key shape for the same logical scope.
const (
	KindRoom   = "room"
	KindVolume = "volume"
)

// RoomKey builds the Manager scope key for a same-Room broadcast bus.
func RoomKey(roomID string) string { return KindRoom + ":" + roomID }

// VolumeKey builds the Manager scope key for a cross-Room volume bus.
func VolumeKey(volumeName string) string { return KindVolume + ":" + volumeName }

// Manager owns every Bus in the daemon. Buses are created lazily on first
// Subscribe / Publish to a scope.
type Manager struct {
	bufSize int

	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	buses map[string]*Bus
}

// New returns a Manager. The supplied context governs every bus spawned by
// the manager — cancel it (or call Shutdown) to wind everything down.
func New(parent context.Context) *Manager {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	return &Manager{
		bufSize: DefaultBufSize,
		ctx:     ctx,
		cancel:  cancel,
		buses:   make(map[string]*Bus),
	}
}

// SetBufSize overrides the default publish-channel buffer for buses created
// after this point. Existing buses are unaffected. Intended for tests.
func (m *Manager) SetBufSize(n int) {
	if n <= 0 {
		n = DefaultBufSize
	}
	m.mu.Lock()
	m.bufSize = n
	m.mu.Unlock()
}

// GetOrCreate returns the Bus for key, creating and starting it if missing.
func (m *Manager) GetOrCreate(key string) *Bus {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.buses[key]; ok {
		return b
	}
	b := newBus(key, m.bufSize)
	m.buses[key] = b
	go b.Run(m.ctx)
	return b
}

// Get returns the Bus for key without creating one. Nil if absent.
func (m *Manager) Get(key string) *Bus {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buses[key]
}

// NewSubID returns an unguessable subscription token. Format is opaque
// to callers — today 16 random bytes hex-encoded behind a "sub-" prefix.
// Random (rather than monotonic) so one Agent cannot trivially infer
// another's sub_id and cancel its subscription.
func (m *Manager) NewSubID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand can't fail on Linux except in pathological cases;
		// panic so the bug surfaces loudly rather than producing a
		// predictable token.
		panic("eventbus: rand.Read: " + err.Error())
	}
	return "sub-" + hex.EncodeToString(b[:])
}

// UnsubscribeOwned removes a sub_id from whichever bus owns it, but only
// if owner matches the Notifier that registered it. See Bus.UnsubscribeOwned
// for the (found, owned) semantics.
func (m *Manager) UnsubscribeOwned(subID string, owner Notifier) (found, owned bool) {
	for _, b := range m.snapshotBuses() {
		if f, o := b.UnsubscribeOwned(subID, owner); f {
			return f, o
		}
	}
	return false, false
}

// UnsubscribeNotifier drops every subscription belonging to n from every
// bus. Called when the Agent exits so subscription state can't leak.
// Pass *agent.Conn here — it satisfies Notifier.
func (m *Manager) UnsubscribeNotifier(n Notifier) {
	if n == nil {
		return
	}
	for _, b := range m.snapshotBuses() {
		b.UnsubscribeNotifier(n)
	}
}

// RemoveScope tears down the bus for key. Subscribers are dropped; pending
// publishes fail with "bus stopped". Future calls to GetOrCreate(key) will
// build a fresh bus — useful for tests, and harmless in prod since a
// Volume that was just removed shouldn't have any live subs anyway.
func (m *Manager) RemoveScope(key string) {
	m.mu.Lock()
	b := m.buses[key]
	delete(m.buses, key)
	m.mu.Unlock()
	if b != nil {
		b.Stop()
	}
}

// Shutdown cancels the manager's context and stops every bus.
func (m *Manager) Shutdown() {
	m.cancel()
	m.mu.Lock()
	bs := make([]*Bus, 0, len(m.buses))
	for _, b := range m.buses {
		bs = append(bs, b)
	}
	m.buses = make(map[string]*Bus)
	m.mu.Unlock()
	for _, b := range bs {
		b.Stop()
	}
}

func (m *Manager) snapshotBuses() []*Bus {
	m.mu.Lock()
	out := make([]*Bus, 0, len(m.buses))
	for _, b := range m.buses {
		out = append(out, b)
	}
	m.mu.Unlock()
	return out
}
