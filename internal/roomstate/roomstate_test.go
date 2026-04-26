package roomstate

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/anne-x/hive/internal/ipc"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	root := t.TempDir()
	in := &Snapshot{
		Name: "myroom",
		Members: []MemberSnap{
			{
				Image:     ipc.ImageRef{Name: "echo", Version: "0.1.0"},
				RankName:  "intern",
				QuotaOver: json.RawMessage(`{"tokens":{"openai:gpt-4o":1000}}`),
				Volumes: []ipc.VolumeMountRef{
					{Name: "kb", Mountpoint: "/kb", Mode: "ro"},
				},
				HiredAt: time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC),
			},
		},
	}
	if err := Save(root, "myroom-1", in); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadAll(root)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(loaded) != 1 {
		t.Fatalf("loaded %d, want 1", len(loaded))
	}
	got := loaded[0]
	if got.RoomID != "myroom-1" {
		t.Errorf("RoomID = %q, want myroom-1", got.RoomID)
	}
	if got.Name != "myroom" || got.Version != CurrentVersion {
		t.Errorf("snapshot meta drift: %+v", got.Snapshot)
	}
	if len(got.Members) != 1 {
		t.Fatalf("members = %d, want 1", len(got.Members))
	}
	m := got.Members[0]
	if m.Image.Name != "echo" || m.Image.Version != "0.1.0" || m.RankName != "intern" {
		t.Errorf("member meta drift: %+v", m)
	}
	if string(m.QuotaOver) == "" || string(m.QuotaOver) == "null" {
		t.Errorf("quota override lost: %q", string(m.QuotaOver))
	}
	if len(m.Volumes) != 1 || m.Volumes[0].Name != "kb" {
		t.Errorf("volumes drift: %+v", m.Volumes)
	}
	// SavedAt should have been stamped by Save (was zero-valued in input).
	if got.SavedAt.IsZero() {
		t.Error("SavedAt not stamped")
	}
}

func TestSaveAtomicNoTempLeftBehind(t *testing.T) {
	root := t.TempDir()
	if err := Save(root, "r1", &Snapshot{Name: "r1"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	dir := filepath.Join(root, "r1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if e.Name() == stateFile {
			continue
		}
		t.Errorf("unexpected residue %q in room dir — temp file not renamed cleanly", e.Name())
	}
}

func TestLoadAllSkipsCorruptAndFutureVersion(t *testing.T) {
	root := t.TempDir()

	// Good entry.
	good := &Snapshot{Name: "good"}
	if err := Save(root, "good-1", good); err != nil {
		t.Fatalf("Save good: %v", err)
	}

	// Corrupt JSON.
	corrupt := filepath.Join(root, "corrupt-1")
	if err := os.MkdirAll(corrupt, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, stateFile), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Future schema version.
	future := filepath.Join(root, "future-1")
	if err := os.MkdirAll(future, 0o750); err != nil {
		t.Fatal(err)
	}
	futureJSON, _ := json.Marshal(map[string]any{
		"version": CurrentVersion + 99,
		"name":    "future",
	})
	if err := os.WriteFile(filepath.Join(future, stateFile), futureJSON, 0o600); err != nil {
		t.Fatal(err)
	}

	// Dir without state.json (e.g. legacy rootfs/logs from pre-persistence).
	bare := filepath.Join(root, "bare-1")
	if err := os.MkdirAll(bare, 0o750); err != nil {
		t.Fatal(err)
	}

	loaded, err := LoadAll(root)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(loaded) != 1 || loaded[0].RoomID != "good-1" {
		t.Fatalf("expected only good-1, got %+v", loaded)
	}
}

func TestLoadAllMissingRootIsEmpty(t *testing.T) {
	loaded, err := LoadAll(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("expected empty, got %d", len(loaded))
	}
}

func TestDeleteIdempotent(t *testing.T) {
	root := t.TempDir()
	if err := Save(root, "r1", &Snapshot{Name: "r1"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := Delete(root, "r1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Second Delete is a no-op.
	if err := Delete(root, "r1"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	// Delete on a never-existed Room is also a no-op.
	if err := Delete(root, "ghost"); err != nil {
		t.Fatalf("ghost Delete: %v", err)
	}
	// state.json is gone but the Room dir survives — recovery's LoadAll
	// will skip it via the read-error branch.
	if _, err := os.Stat(filepath.Join(root, "r1", stateFile)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("state.json should be gone, stat err = %v", err)
	}
}

func TestSaveConcurrentSameRoom(t *testing.T) {
	// Two writers racing to update the same Room must serialise — neither
	// should error and the final on-disk state must be one of the two
	// snapshots in full (no interleaving).
	root := t.TempDir()
	const n = 32

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = Save(root, "race", &Snapshot{
				Name: "race",
				Members: []MemberSnap{
					{Image: ipc.ImageRef{Name: "agent", Version: "v1"}, HiredAt: time.Now()},
				},
			})
		}(i)
	}
	wg.Wait()

	loaded, err := LoadAll(root)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(loaded) != 1 || loaded[0].RoomID != "race" {
		t.Fatalf("expected one race snapshot, got %+v", loaded)
	}
	if len(loaded[0].Members) != 1 {
		t.Fatalf("expected 1 member, got %d", len(loaded[0].Members))
	}
}

func TestSaveAssignsVersionWhenZero(t *testing.T) {
	root := t.TempDir()
	in := &Snapshot{Name: "v0"}
	if in.Version != 0 {
		t.Fatalf("test setup: input Version should start at 0")
	}
	if err := Save(root, "v0-1", in); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if in.Version != CurrentVersion {
		t.Errorf("Save did not stamp Version, got %d", in.Version)
	}
}

func TestSaveNilRejected(t *testing.T) {
	if err := Save(t.TempDir(), "x", nil); err == nil {
		t.Fatal("expected error for nil snapshot")
	}
}
