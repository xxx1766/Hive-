package conversation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// roomsDir gives each test a private temp dir.
func roomsDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	return d
}

func TestCreateAndLoad(t *testing.T) {
	s := NewStore(roomsDir(t))
	c := &Conversation{
		ID:            "c1",
		RoomID:        "room-A",
		InitialTarget: "writer",
		InitialInput:  json.RawMessage(`{"section":"design"}`),
	}
	if err := s.Create(c); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.MaxRounds != DefaultMaxRounds {
		t.Errorf("MaxRounds default not applied: %d", c.MaxRounds)
	}
	if c.Status != StatusPlanned {
		t.Errorf("Status default: %s", c.Status)
	}
	if c.Tag == "" {
		t.Errorf("Tag should auto-generate")
	}
	if c.Version != CurrentVersion {
		t.Errorf("Version not set: %d", c.Version)
	}

	loaded, err := s.Load("room-A", "c1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ID != "c1" || loaded.InitialTarget != "writer" {
		t.Errorf("Load returned wrong record: %+v", loaded)
	}
}

func TestCreateRejectsDuplicate(t *testing.T) {
	s := NewStore(roomsDir(t))
	c := &Conversation{ID: "dup", RoomID: "room-A", InitialTarget: "x"}
	if err := s.Create(c); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	c2 := &Conversation{ID: "dup", RoomID: "room-A", InitialTarget: "x"}
	if err := s.Create(c2); err == nil {
		t.Fatal("expected duplicate Create to fail")
	}
}

func TestAppendIncrementsRoundForPeerOnly(t *testing.T) {
	s := NewStore(roomsDir(t))
	c := &Conversation{ID: "c2", RoomID: "room-A", InitialTarget: "writer"}
	if err := s.Create(c); err != nil {
		t.Fatal(err)
	}
	// task_input doesn't bump round.
	if _, err := s.Append("room-A", "c2", Message{From: "", To: "writer", Kind: KindTaskInput}); err != nil {
		t.Fatal(err)
	}
	// peer hops bump.
	for i := 1; i <= 3; i++ {
		got, err := s.Append("room-A", "c2", Message{From: "writer", To: "outline", Kind: KindPeer})
		if err != nil {
			t.Fatal(err)
		}
		if got.RoundCount != i {
			t.Errorf("round %d: got RoundCount=%d", i, got.RoundCount)
		}
	}
	loaded, _ := s.Load("room-A", "c2")
	if loaded.RoundCount != 3 {
		t.Errorf("persisted RoundCount=%d want 3", loaded.RoundCount)
	}
	if len(loaded.Messages) != 4 {
		t.Errorf("persisted messages=%d want 4", len(loaded.Messages))
	}
	// Participants tracked.
	if len(loaded.Participants) != 2 {
		t.Errorf("participants=%v", loaded.Participants)
	}
	// Message IDs are monotonic.
	for i, m := range loaded.Messages {
		if m.ID != "m"+string(rune('1'+i)) {
			t.Errorf("msg[%d].ID=%q", i, m.ID)
		}
	}
}

func TestUpdateStatus(t *testing.T) {
	s := NewStore(roomsDir(t))
	c := &Conversation{ID: "c3", RoomID: "room-A", InitialTarget: "writer"}
	_ = s.Create(c)

	updated, err := s.Update("room-A", "c3", func(cc *Conversation) {
		cc.Status = StatusActive
		cc.StartedAt = time.Now().UTC()
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != StatusActive {
		t.Errorf("status=%s", updated.Status)
	}

	// Reload to verify persistence.
	reloaded, _ := s.Load("room-A", "c3")
	if reloaded.Status != StatusActive || reloaded.StartedAt.IsZero() {
		t.Errorf("persisted state wrong: %+v", reloaded)
	}
}

func TestUpdatePreservesIDAndRoom(t *testing.T) {
	s := NewStore(roomsDir(t))
	c := &Conversation{ID: "c4", RoomID: "room-A", InitialTarget: "writer"}
	_ = s.Create(c)

	updated, err := s.Update("room-A", "c4", func(cc *Conversation) {
		cc.ID = "evil"
		cc.RoomID = "room-B"
		cc.Status = StatusFailed
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != "c4" || updated.RoomID != "room-A" {
		t.Errorf("Update let mutator change immutable fields: %+v", updated)
	}
	if updated.Status != StatusFailed {
		t.Errorf("Update lost status change: %s", updated.Status)
	}
}

func TestListByRoomSortedByCreatedAt(t *testing.T) {
	s := NewStore(roomsDir(t))
	for i, id := range []string{"first", "second", "third"} {
		c := &Conversation{
			ID:            id,
			RoomID:        "room-A",
			InitialTarget: "x",
			CreatedAt:     time.Now().UTC().Add(time.Duration(i) * time.Millisecond),
		}
		if err := s.Create(c); err != nil {
			t.Fatal(err)
		}
	}
	out, err := s.ListByRoom("room-A")
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("want 3 got %d", len(out))
	}
	for i, want := range []string{"first", "second", "third"} {
		if out[i].ID != want {
			t.Errorf("[%d] = %s want %s", i, out[i].ID, want)
		}
	}
}

func TestListByRoomMissingDirIsNil(t *testing.T) {
	s := NewStore(roomsDir(t))
	out, err := s.ListByRoom("nonexistent")
	if err != nil || out != nil {
		t.Errorf("missing room: out=%v err=%v", out, err)
	}
}

func TestConcurrentAppend(t *testing.T) {
	s := NewStore(roomsDir(t))
	c := &Conversation{ID: "c5", RoomID: "room-A", InitialTarget: "x", MaxRounds: 1000}
	_ = s.Create(c)

	const N = 50
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Append("room-A", "c5", Message{From: "writer", To: "outline", Kind: KindPeer})
		}()
	}
	wg.Wait()

	loaded, _ := s.Load("room-A", "c5")
	if len(loaded.Messages) != N {
		t.Errorf("messages=%d want %d", len(loaded.Messages), N)
	}
	if loaded.RoundCount != N {
		t.Errorf("round count=%d want %d", loaded.RoundCount, N)
	}
	// Messages must be uniquely numbered (m1..mN) — no lost IDs.
	seen := map[string]bool{}
	for _, m := range loaded.Messages {
		if seen[m.ID] {
			t.Errorf("duplicate message ID: %s", m.ID)
		}
		seen[m.ID] = true
	}
}

func TestLoadRejectsHigherVersion(t *testing.T) {
	s := NewStore(roomsDir(t))
	dir := filepath.Join(s.roomsDir, "room-A", ConversationsSubdir)
	_ = os.MkdirAll(dir, 0o755)
	// Write a forward-version record by hand.
	data, _ := json.Marshal(&Conversation{
		Version: CurrentVersion + 100,
		ID:      "future",
		RoomID:  "room-A",
		Status:  StatusPlanned,
	})
	_ = os.WriteFile(filepath.Join(dir, "future.json"), data, 0o644)

	if _, err := s.Load("room-A", "future"); err == nil {
		t.Fatal("expected version-too-new rejection")
	}
}

func TestPartialFileSurvivesRename(t *testing.T) {
	// Verify that a half-written tempfile in the conversations dir
	// doesn't poison ListByRoom — it should appear as an unreadable
	// file (but not as a Conversation).
	s := NewStore(roomsDir(t))
	c := &Conversation{ID: "c6", RoomID: "room-A", InitialTarget: "x"}
	_ = s.Create(c)

	// Drop a leftover temp file (simulates a crash mid-write).
	dir := filepath.Join(s.roomsDir, "room-A", ConversationsSubdir)
	_ = os.WriteFile(filepath.Join(dir, ".c6-partial.json"), []byte("{not json"), 0o644)

	// Real conversation still lists.
	out, _ := s.ListByRoom("room-A")
	if len(out) != 1 || out[0].ID != "c6" {
		t.Errorf("partial-file noise broke listing: %+v", out)
	}
}

func TestMarkActiveAsInterrupted(t *testing.T) {
	s := NewStore(roomsDir(t))
	// Three conversations: one active, one done, one planned.
	for id, status := range map[string]Status{"running": StatusActive, "old": StatusDone, "queued": StatusPlanned} {
		c := &Conversation{ID: id, RoomID: "room-A", InitialTarget: "x"}
		_ = s.Create(c)
		if status != StatusPlanned {
			_, _ = s.Update("room-A", id, func(cc *Conversation) { cc.Status = status })
		}
	}
	n, err := s.MarkActiveAsInterrupted()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("changed=%d want 1", n)
	}
	for id, want := range map[string]Status{"running": StatusInterrupted, "old": StatusDone, "queued": StatusPlanned} {
		got, _ := s.Load("room-A", id)
		if got.Status != want {
			t.Errorf("%s: got %s want %s", id, got.Status, want)
		}
	}
}

func TestStatusTerminal(t *testing.T) {
	for _, s := range []Status{StatusDone, StatusFailed, StatusCancelled, StatusInterrupted} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	for _, s := range []Status{StatusPlanned, StatusActive} {
		if s.Terminal() {
			t.Errorf("%s should not be terminal", s)
		}
	}
}

func TestSummarize(t *testing.T) {
	c := &Conversation{
		ID: "c7", RoomID: "r", Tag: "smoke", Status: StatusActive,
		InitialTarget: "writer", MaxRounds: 8, RoundCount: 3,
		Messages:  []Message{{}, {}, {}},
		CreatedAt: time.Unix(100, 0),
	}
	sum := c.Summarize()
	if sum.ID != "c7" || sum.MessageCount != 3 || sum.RoundCount != 3 {
		t.Errorf("Summarize: %+v", sum)
	}
}

// TestSummary_PreservesMembers locks the projection: cross-Room conv
// summaries carry Members through so the kanban can render the
// "↔ N rooms" badge without fetching the full transcript per card.
func TestSummary_PreservesMembers(t *testing.T) {
	c := &Conversation{
		ID: "x", RoomID: "owner", InitialTarget: "a",
		Members: []Member{
			{RoomID: "owner", AgentName: "a"},
			{RoomID: "other", AgentName: "b"},
		},
	}
	sum := c.Summarize()
	if len(sum.Members) != 2 {
		t.Fatalf("Members lost: %+v", sum.Members)
	}
	if sum.Members[0] != (Member{RoomID: "owner", AgentName: "a"}) ||
		sum.Members[1] != (Member{RoomID: "other", AgentName: "b"}) {
		t.Errorf("Members order/contents wrong: %+v", sum.Members)
	}

	// And the omitempty contract: no Members ⇒ nil slice in JSON.
	plain := (&Conversation{ID: "y", RoomID: "owner", InitialTarget: "a"}).Summarize()
	if plain.Members != nil {
		t.Errorf("expected nil Members for legacy conv, got %+v", plain.Members)
	}
}

func TestLoadByID_FindsAcrossRooms(t *testing.T) {
	s := NewStore(roomsDir(t))
	if err := s.Create(&Conversation{ID: "x1", RoomID: "room-A", InitialTarget: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Create(&Conversation{ID: "x2", RoomID: "room-B", InitialTarget: "b"}); err != nil {
		t.Fatal(err)
	}
	c, err := s.LoadByID("x2")
	if err != nil {
		t.Fatalf("LoadByID(x2): %v", err)
	}
	if c.RoomID != "room-B" {
		t.Errorf("expected owner room-B, got %s", c.RoomID)
	}
	if _, err := s.LoadByID("nonexistent"); !os.IsNotExist(err) {
		t.Errorf("expected ErrNotExist, got %v", err)
	}
}

func TestIndexByID(t *testing.T) {
	s := NewStore(roomsDir(t))
	for _, kv := range []struct{ id, room string }{
		{"a", "room-1"}, {"b", "room-2"}, {"c", "room-1"},
	} {
		if err := s.Create(&Conversation{ID: kv.id, RoomID: kv.room, InitialTarget: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	idx, err := s.IndexByID()
	if err != nil {
		t.Fatalf("IndexByID: %v", err)
	}
	if idx["a"] != "room-1" || idx["b"] != "room-2" || idx["c"] != "room-1" {
		t.Errorf("index wrong: %+v", idx)
	}
	if len(idx) != 3 {
		t.Errorf("index size: got %d want 3", len(idx))
	}
}

func TestIsCrossRoom_AndFindMember(t *testing.T) {
	cases := []struct {
		members []Member
		want    bool
	}{
		{nil, false},
		{[]Member{{RoomID: "A", AgentName: "x"}}, false},
		{[]Member{{RoomID: "A", AgentName: "x"}, {RoomID: "A", AgentName: "y"}}, false},
		{[]Member{{RoomID: "A", AgentName: "x"}, {RoomID: "B", AgentName: "y"}}, true},
	}
	for i, tc := range cases {
		c := &Conversation{Members: tc.members}
		if got := c.IsCrossRoom(); got != tc.want {
			t.Errorf("case %d IsCrossRoom: got %v want %v", i, got, tc.want)
		}
	}
	c := &Conversation{Members: []Member{
		{RoomID: "A", AgentName: "writer"},
		{RoomID: "B", AgentName: "reviewer"},
	}}
	if m := c.FindMember("reviewer"); m == nil || m.RoomID != "B" {
		t.Errorf("FindMember(reviewer): %+v", m)
	}
	if m := c.FindMember("missing"); m != nil {
		t.Errorf("FindMember(missing) should return nil, got %+v", m)
	}
}
