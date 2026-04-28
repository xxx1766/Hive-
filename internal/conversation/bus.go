package conversation

import (
	"sync"
	"time"
)

// EventType labels Bus events. UI/SSE clients route off this string; the
// set is open so adding new types never breaks older clients.
type EventType string

const (
	EventConvCreated   EventType = "conversation.created"
	EventConvStarted   EventType = "conversation.started"
	EventConvMessage   EventType = "conversation.message"
	EventConvStatusUpd EventType = "conversation.status"
	EventConvFinished  EventType = "conversation.finished"
)

// Event is what subscribers receive. Payload is whatever the publisher
// chose — for status events it's the new Conversation snapshot, for
// message events it's the new Message, etc.
type Event struct {
	Type    EventType `json:"type"`
	RoomID  string    `json:"room_id"`
	ConvID  string    `json:"conv_id,omitempty"`
	Payload any       `json:"payload,omitempty"`
	TS      time.Time `json:"ts"`
}

// Bus is a per-Room pub-sub. Subscribers get a buffered channel; if a
// subscriber falls behind by more than busSubBuffer events the publish
// drops the event for that subscriber (UI lag shouldn't slow the daemon).
//
// Lifecycle: callers Subscribe, get back a channel + cancel func, range
// over the channel, and call cancel when done. Cancel is idempotent.
type Bus struct {
	mu     sync.Mutex
	subs   map[string]map[uint64]chan Event // roomID → subID → channel
	nextID uint64
}

// busSubBuffer is the per-subscriber channel depth. Events overflowing
// this get dropped for the slow subscriber — but other subs and the
// publisher itself stay unblocked. UIs that want guaranteed delivery
// should fall back to polling Store.ListByRoom.
const busSubBuffer = 64

// NewBus returns a fresh Bus with no subscribers.
func NewBus() *Bus {
	return &Bus{subs: map[string]map[uint64]chan Event{}}
}

// Publish fans out e to every active subscriber of e.RoomID. Non-
// blocking: a subscriber whose buffer is full silently misses this event.
func (b *Bus) Publish(e Event) {
	if e.TS.IsZero() {
		e.TS = time.Now().UTC()
	}
	b.mu.Lock()
	subs := b.subs[e.RoomID]
	chans := make([]chan Event, 0, len(subs))
	for _, ch := range subs {
		chans = append(chans, ch)
	}
	b.mu.Unlock()

	for _, ch := range chans {
		select {
		case ch <- e:
		default:
			// Subscriber buffer full — drop.
		}
	}
}

// Subscribe returns a channel of events scoped to roomID and a cancel
// func that closes the channel + unregisters. The channel is closed
// exactly once; calling the cancel func twice is safe.
func (b *Bus) Subscribe(roomID string) (<-chan Event, func()) {
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	ch := make(chan Event, busSubBuffer)
	if b.subs[roomID] == nil {
		b.subs[roomID] = map[uint64]chan Event{}
	}
	b.subs[roomID][id] = ch
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if room, ok := b.subs[roomID]; ok {
				delete(room, id)
				if len(room) == 0 {
					delete(b.subs, roomID)
				}
			}
			b.mu.Unlock()
			close(ch)
		})
	}
	return ch, cancel
}

// SubscriberCount reports how many active subscribers currently watch
// roomID. Mainly useful for tests.
func (b *Bus) SubscriberCount(roomID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[roomID])
}
