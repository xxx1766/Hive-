package conversation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ConversationsSubdir is the per-Room directory holding conversation
// JSON files. Sits next to the existing logs/, rootfs/, workspace/ and
// state.json under <roomsDir>/<roomID>/.
const ConversationsSubdir = "conversations"

// Store persists Conversations on disk. Roots at <RoomsDir> (the same
// path roomstate uses), so cleanup paths line up: removing a Room dir
// removes its conversations too.
type Store struct {
	roomsDir string

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-conv mutex, keyed by ConvID
}

// NewStore returns a Store rooted at roomsDir. The dir doesn't have to
// exist yet — Save() creates it lazily.
func NewStore(roomsDir string) *Store {
	return &Store{roomsDir: roomsDir, locks: map[string]*sync.Mutex{}}
}

func (s *Store) lockFor(convID string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.locks[convID]
	if !ok {
		l = &sync.Mutex{}
		s.locks[convID] = l
	}
	return l
}

func (s *Store) dirFor(roomID string) string {
	return filepath.Join(s.roomsDir, roomID, ConversationsSubdir)
}

func (s *Store) pathFor(roomID, convID string) string {
	return filepath.Join(s.dirFor(roomID), convID+".json")
}

// Create writes a new Conversation. Fails if convID already exists —
// callers should generate fresh IDs (NewID below).
func (s *Store) Create(c *Conversation) error {
	if c == nil || c.ID == "" || c.RoomID == "" {
		return errors.New("conversation: nil or missing id/room")
	}
	if c.MaxRounds <= 0 {
		c.MaxRounds = DefaultMaxRounds
	}
	if c.Status == "" {
		c.Status = StatusPlanned
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	if c.Tag == "" {
		c.Tag = fmt.Sprintf("%s@%d", c.InitialTarget, c.CreatedAt.Unix())
	}
	c.Version = CurrentVersion

	l := s.lockFor(c.ID)
	l.Lock()
	defer l.Unlock()

	if _, err := os.Stat(s.pathFor(c.RoomID, c.ID)); err == nil {
		return fmt.Errorf("conversation: %s already exists", c.ID)
	}
	return s.writeLocked(c)
}

// Append adds a message and returns the updated Conversation. Caller-
// supplied msg.ID/Round/TS are overwritten — this guarantees monotonic
// IDs and a single time source even across racing writers.
func (s *Store) Append(roomID, convID string, msg Message) (*Conversation, error) {
	l := s.lockFor(convID)
	l.Lock()
	defer l.Unlock()

	c, err := s.loadLocked(roomID, convID)
	if err != nil {
		return nil, err
	}
	msg.ID = "m" + strconv.Itoa(len(c.Messages)+1)
	msg.ConvID = convID
	msg.TS = time.Now().UTC()
	if msg.Kind == KindPeer {
		msg.Round = c.RoundCount + 1
		c.RoundCount = msg.Round
	}
	c.Messages = append(c.Messages, msg)
	c.addParticipant(msg.From)
	c.addParticipant(msg.To)

	if err := s.writeLocked(c); err != nil {
		return nil, err
	}
	return c, nil
}

// Update applies mut under the per-conv lock and persists. mut may
// modify any field; ID/RoomID/Version are restored after for safety.
func (s *Store) Update(roomID, convID string, mut func(*Conversation)) (*Conversation, error) {
	l := s.lockFor(convID)
	l.Lock()
	defer l.Unlock()

	c, err := s.loadLocked(roomID, convID)
	if err != nil {
		return nil, err
	}
	origID, origRoom := c.ID, c.RoomID
	mut(c)
	c.ID = origID
	c.RoomID = origRoom
	c.Version = CurrentVersion

	if err := s.writeLocked(c); err != nil {
		return nil, err
	}
	return c, nil
}

// Load returns a single Conversation by id. Returns os.ErrNotExist if
// the file is missing.
func (s *Store) Load(roomID, convID string) (*Conversation, error) {
	l := s.lockFor(convID)
	l.Lock()
	defer l.Unlock()
	return s.loadLocked(roomID, convID)
}

// ListByRoom returns every Conversation under the Room, sorted oldest →
// newest by CreatedAt. A missing Room dir yields nil/nil — that's the
// pre-first-conversation state, not an error.
func (s *Store) ListByRoom(roomID string) ([]*Conversation, error) {
	dir := s.dirFor(roomID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []*Conversation
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		convID := e.Name()[:len(e.Name())-len(".json")]
		c, err := s.Load(roomID, convID)
		if err != nil {
			// Skip unreadable / corrupt files — analogous to roomstate.LoadAll.
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// LoadByID resolves a Conversation by ID alone, scanning every Room
// directory under roomsDir until a match is found. Used by the
// daemon's cross-Room routing path: when an agent in Room A peer_sends
// on a conv whose owner is Room B, A's PeerSend handler can't easily
// pass the right roomID to Load — LoadByID closes that gap.
//
// Slow on cold cache: O(rooms) directory listings. The daemon wraps
// this with an in-memory convID→roomID index (see internal/daemon/
// conv_index.go) so steady-state lookups are O(1).
//
// Returns os.ErrNotExist when the ID isn't found in any Room.
func (s *Store) LoadByID(convID string) (*Conversation, error) {
	rooms, err := os.ReadDir(s.roomsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	for _, r := range rooms {
		if !r.IsDir() {
			continue
		}
		c, err := s.Load(r.Name(), convID)
		if err == nil {
			return c, nil
		}
		if !os.IsNotExist(err) {
			// Real error reading this candidate (corrupt JSON, permission
			// denied, etc.) — keep scanning others; don't let a single
			// bad file mask a valid one.
			continue
		}
	}
	return nil, os.ErrNotExist
}

// IndexByID enumerates every Conversation under roomsDir and returns
// a convID→ownerRoomID map. Called once at daemon startup so the
// in-memory index can answer cross-Room peer_send queries in O(1) for
// the rest of the process lifetime. Subsequent Create() calls update
// the index incrementally (handled in the daemon, not here).
func (s *Store) IndexByID() (map[string]string, error) {
	rooms, err := os.ReadDir(s.roomsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}, nil
		}
		return nil, err
	}
	out := map[string]string{}
	for _, r := range rooms {
		if !r.IsDir() {
			continue
		}
		dir := s.dirFor(r.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
				continue
			}
			id := e.Name()[:len(e.Name())-len(".json")]
			out[id] = r.Name()
		}
	}
	return out, nil
}

// MarkActiveAsInterrupted sweeps roomsDir on daemon startup and flips
// any conversations stuck in "active" to "interrupted" — the prior
// daemon process died holding their state, so no in-flight runner can
// converge them. Returns the count of changed records.
func (s *Store) MarkActiveAsInterrupted() (int, error) {
	rooms, err := os.ReadDir(s.roomsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	changed := 0
	for _, r := range rooms {
		if !r.IsDir() {
			continue
		}
		convs, err := s.ListByRoom(r.Name())
		if err != nil {
			continue
		}
		for _, c := range convs {
			if c.Status != StatusActive {
				continue
			}
			_, err := s.Update(r.Name(), c.ID, func(cc *Conversation) {
				cc.Status = StatusInterrupted
				cc.FinishedAt = time.Now().UTC()
				cc.Error = "daemon restarted while conversation was active"
			})
			if err == nil {
				changed++
			}
		}
	}
	return changed, nil
}

// loadLocked reads + parses without taking the lock — caller must hold
// the per-conv mutex (Load wraps it; Append/Update reuse it).
func (s *Store) loadLocked(roomID, convID string) (*Conversation, error) {
	data, err := os.ReadFile(s.pathFor(roomID, convID))
	if err != nil {
		return nil, err
	}
	var c Conversation
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("conversation: parse %s: %w", convID, err)
	}
	if c.Version > CurrentVersion {
		return nil, fmt.Errorf("conversation: %s version %d > current %d", convID, c.Version, CurrentVersion)
	}
	return &c, nil
}

// writeLocked atomically writes c — caller must hold the per-conv mutex.
func (s *Store) writeLocked(c *Conversation) error {
	dir := s.dirFor(c.RoomID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("conversation: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("conversation: marshal: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "."+c.ID+"-*.json")
	if err != nil {
		return fmt.Errorf("conversation: tempfile: %w", err)
	}
	tmpPath := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpPath) }

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("conversation: write: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("conversation: close: %w", err)
	}
	if err := os.Rename(tmpPath, s.pathFor(c.RoomID, c.ID)); err != nil {
		cleanup()
		return fmt.Errorf("conversation: rename: %w", err)
	}
	return nil
}

// NewID mints a fresh Conversation id. The "conv-" prefix makes IDs
// trivially distinguishable from RoomIDs in logs / SSE / URLs.
func NewID() string {
	return fmt.Sprintf("conv-%d-%d", time.Now().UnixNano(), nextSeq())
}

var (
	seqMu sync.Mutex
	seq   int64
)

func nextSeq() int64 {
	seqMu.Lock()
	defer seqMu.Unlock()
	seq++
	return seq
}
