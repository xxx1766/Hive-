package daemon

import "sync"

// convIndex maps convID → owner-RoomID for O(1) cross-Room conv lookups.
//
// Without this index, the daemon's PeerSendForward hook would need to
// scan every Room's conversations/ directory on each peer hop — fine
// for tens of conversations, painful at hundreds. The index is built
// once at startup from conversation.Store.IndexByID() and updated
// incrementally on every Create.
//
// Why not just store ownerRoomID inline on each Conversation? It IS
// stored on the file (as RoomID), but we don't always know which Room
// to ask for the file. The index is the "phone book" that turns
// "convID = c-42" into "look in Room A's directory".
type convIndex struct {
	mu sync.RWMutex
	m  map[string]string
}

func newConvIndex(seed map[string]string) *convIndex {
	idx := &convIndex{m: map[string]string{}}
	for k, v := range seed {
		idx.m[k] = v
	}
	return idx
}

// Owner returns the RoomID that owns convID, or "" if unknown.
func (i *convIndex) Owner(convID string) string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.m[convID]
}

// Set records owner for convID. Used after a successful Create. Safe
// to call on an existing entry — overwrites; conv IDs are globally
// unique so collision means one of them is being replaced (e.g. a
// recovery sweep loaded the same id from disk again).
func (i *convIndex) Set(convID, roomID string) {
	i.mu.Lock()
	i.m[convID] = roomID
	i.mu.Unlock()
}

// Forget drops convID. No-op when absent. Called when a conv's owner
// Room is stopped/removed and there's no recovery scenario.
func (i *convIndex) Forget(convID string) {
	i.mu.Lock()
	delete(i.m, convID)
	i.mu.Unlock()
}

// Snapshot returns a defensive copy of the map for tests / debugging.
func (i *convIndex) Snapshot() map[string]string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make(map[string]string, len(i.m))
	for k, v := range i.m {
		out[k] = v
	}
	return out
}
