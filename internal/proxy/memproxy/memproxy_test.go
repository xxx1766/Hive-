package memproxy

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/rank"
	"github.com/anne-x/hive/internal/rpc"
	"github.com/anne-x/hive/internal/volume"
)

func setup(t *testing.T) (*Proxy, *volume.Manager, string) {
	t.Helper()
	root := t.TempDir()
	volMgr, err := volume.New(filepath.Join(root, "volumes"))
	if err != nil {
		t.Fatal(err)
	}
	roomsDir := filepath.Join(root, "rooms")
	p := &Proxy{
		RoomID:    "room-A",
		AgentName: "tester",
		Rank:      &rank.Rank{Name: "staff", MemoryAllowed: true},
		Volumes:   volMgr,
		RoomsDir:  roomsDir,
	}
	return p, volMgr, roomsDir
}

func enc(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPutGet_PrivateScope(t *testing.T) {
	p, _, _ := setup(t)

	_, err := p.Put(enc(t, rpc.MemoryPutParams{Key: "hello", Value: []byte("world")}))
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.Get(enc(t, rpc.MemoryGetParams{Key: "hello"}))
	if err != nil {
		t.Fatal(err)
	}
	res := got.(rpc.MemoryGetResult)
	if !res.Exists || string(res.Value) != "world" {
		t.Fatalf("roundtrip failed: %+v", res)
	}
}

func TestGet_Missing_NotAnError(t *testing.T) {
	p, _, _ := setup(t)
	got, err := p.Get(enc(t, rpc.MemoryGetParams{Key: "ghost"}))
	if err != nil {
		t.Fatal(err)
	}
	res := got.(rpc.MemoryGetResult)
	if res.Exists {
		t.Fatal("ghost key reported as existing")
	}
}

func TestPutGet_SharedScope_RequiresVolume(t *testing.T) {
	p, volMgr, _ := setup(t)
	if _, err := p.Put(enc(t, rpc.MemoryPutParams{Scope: "missing", Key: "k", Value: []byte("v")})); err == nil {
		t.Fatal("put to missing volume should fail")
	}
	if _, err := volMgr.Create("shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Put(enc(t, rpc.MemoryPutParams{Scope: "shared", Key: "k", Value: []byte("v")})); err != nil {
		t.Fatal(err)
	}
	got, err := p.Get(enc(t, rpc.MemoryGetParams{Scope: "shared", Key: "k"}))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.(rpc.MemoryGetResult).Value) != "v" {
		t.Fatalf("value mismatch: %+v", got)
	}
}

func TestCrossRoom_SharedVolumeVisible(t *testing.T) {
	// Two proxies = two "Rooms" sharing the same volume manager.
	p1, volMgr, _ := setup(t)
	if _, err := volMgr.Create("kb"); err != nil {
		t.Fatal(err)
	}
	if _, err := p1.Put(enc(t, rpc.MemoryPutParams{
		Scope: "kb", Key: "fact-1", Value: []byte("OpenAI GPT-4o context = 128k"),
	})); err != nil {
		t.Fatal(err)
	}

	// Room B: same volumes manager, different RoomID.
	p2 := *p1
	p2.RoomID = "room-B"

	got, err := p2.Get(enc(t, rpc.MemoryGetParams{Scope: "kb", Key: "fact-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.(rpc.MemoryGetResult).Value) != "OpenAI GPT-4o context = 128k" {
		t.Fatal("room B did not see room A's write")
	}
}

func TestPrivateScope_RoomsIsolated(t *testing.T) {
	p1, _, _ := setup(t)
	_, _ = p1.Put(enc(t, rpc.MemoryPutParams{Key: "secret", Value: []byte("A")}))

	p2 := *p1
	p2.RoomID = "room-B"

	got, err := p2.Get(enc(t, rpc.MemoryGetParams{Key: "secret"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.(rpc.MemoryGetResult).Exists {
		t.Fatal("private scope leaked across rooms")
	}
}

func TestList_PrefixFiltered(t *testing.T) {
	p, _, _ := setup(t)
	for _, k := range []string{"user:alice", "user:bob", "doc:paper", "user:charlie"} {
		_, _ = p.Put(enc(t, rpc.MemoryPutParams{Key: k, Value: []byte("v")}))
	}
	got, err := p.List(enc(t, rpc.MemoryListParams{Prefix: "user:"}))
	if err != nil {
		t.Fatal(err)
	}
	keys := got.(rpc.MemoryListResult).Keys
	sort.Strings(keys)
	want := []string{"user:alice", "user:bob", "user:charlie"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("got %v, want %v", keys, want)
	}
}

func TestDelete_Idempotent(t *testing.T) {
	p, _, _ := setup(t)
	_, _ = p.Put(enc(t, rpc.MemoryPutParams{Key: "x", Value: []byte("y")}))
	if _, err := p.Delete(enc(t, rpc.MemoryDeleteParams{Key: "x"})); err != nil {
		t.Fatal(err)
	}
	// Second delete must not error.
	if _, err := p.Delete(enc(t, rpc.MemoryDeleteParams{Key: "x"})); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	// Get must now return Exists=false.
	got, _ := p.Get(enc(t, rpc.MemoryGetParams{Key: "x"}))
	if got.(rpc.MemoryGetResult).Exists {
		t.Fatal("deleted key should not exist")
	}
}

func TestRankGate(t *testing.T) {
	p, _, _ := setup(t)
	p.Rank = &rank.Rank{Name: "intern", MemoryAllowed: false}
	_, err := p.Put(enc(t, rpc.MemoryPutParams{Key: "k", Value: []byte("v")}))
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrCodePermissionDenied {
		t.Fatalf("want permission_denied, got %v", err)
	}
}

func TestValidateKey(t *testing.T) {
	p, _, _ := setup(t)
	bads := []string{"", string(make([]byte, MaxKeyLen+1))}
	for _, k := range bads {
		_, err := p.Put(enc(t, rpc.MemoryPutParams{Key: k, Value: []byte("v")}))
		if err == nil {
			t.Errorf("expected rejection for key %q", k)
		}
	}
}

func TestKeysWithSpecialChars_RoundTrip(t *testing.T) {
	// keys may contain /, spaces, colons, unicode — filename encoding
	// must survive round trips through List.
	p, _, _ := setup(t)
	keys := []string{"a/b", "with space", "用户:张三", "json://path", "weird%char"}
	for _, k := range keys {
		if _, err := p.Put(enc(t, rpc.MemoryPutParams{Key: k, Value: []byte("v")})); err != nil {
			t.Fatalf("put %q: %v", k, err)
		}
	}
	got, err := p.List(enc(t, rpc.MemoryListParams{}))
	if err != nil {
		t.Fatal(err)
	}
	listed := got.(rpc.MemoryListResult).Keys
	sort.Strings(listed)
	sort.Strings(keys)
	if !reflect.DeepEqual(listed, keys) {
		t.Fatalf("round-trip failed:\n got  %v\n want %v", listed, keys)
	}
}
