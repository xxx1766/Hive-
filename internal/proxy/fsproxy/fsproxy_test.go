package fsproxy

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/rank"
	"github.com/anne-x/hive/internal/rpc"
)

// makeProxy returns a Proxy whose rootfs is a fresh temp dir. The rank is
// "manager"-esque: reads anywhere, writes under /data and /tmp.
func makeProxy(t *testing.T) (*Proxy, string) {
	t.Helper()
	rootfs := t.TempDir()
	_ = os.MkdirAll(filepath.Join(rootfs, "data"), 0o755)
	_ = os.MkdirAll(filepath.Join(rootfs, "app"), 0o755)
	return &Proxy{
		RoomRootfs: rootfs,
		Rank: &rank.Rank{
			Name:    "test",
			FSRead:  []string{"/"},
			FSWrite: []string{"/data", "/tmp"},
		},
	}, rootfs
}

func encode(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReadWithinRootfs(t *testing.T) {
	p, rootfs := makeProxy(t)
	f := filepath.Join(rootfs, "data", "hello.txt")
	os.WriteFile(f, []byte("hi"), 0o644)

	got, err := p.Read(encode(t, rpc.FsReadParams{Path: "/data/hello.txt"}))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got.(rpc.FsReadResult).Data) != "hi" {
		t.Fatalf("body: %v", got)
	}
}

func TestReadEscapeAttempt(t *testing.T) {
	// An Agent asks for /data/../../../../etc/passwd — after Clean this
	// becomes /etc/passwd, which is outside the Rank's FSRead list (here
	// the rank allows / so read WOULD succeed if we resolved naively);
	// but the resolve step clamps to rootfs, so the actual file read is
	// <rootfs>/etc/passwd, which doesn't exist and is not a path leak.
	p, _ := makeProxy(t)
	_, err := p.Read(encode(t, rpc.FsReadParams{Path: "/data/../../../../etc/passwd"}))
	if err == nil {
		t.Fatal("expected error, path shouldn't exist inside rootfs")
	}
	// What we specifically don't want: the call resolving to the REAL /etc/passwd.
	// If it did, the body would contain "root:" and err would be nil.
}

func TestReadRankDenied(t *testing.T) {
	p, rootfs := makeProxy(t)
	os.WriteFile(filepath.Join(rootfs, "data", "x"), []byte("y"), 0o644)

	// Narrow the rank so /data isn't readable.
	p.Rank = &rank.Rank{Name: "intern", FSRead: []string{"/app"}}

	_, err := p.Read(encode(t, rpc.FsReadParams{Path: "/data/x"}))
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrCodePermissionDenied {
		t.Fatalf("want permission_denied, got %v", err)
	}
}

func TestWriteRankDenied(t *testing.T) {
	p, _ := makeProxy(t)
	p.Rank = &rank.Rank{Name: "intern", FSWrite: []string{"/tmp"}}

	_, err := p.Write(encode(t, rpc.FsWriteParams{Path: "/data/x", Data: []byte("z")}))
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrCodePermissionDenied {
		t.Fatalf("want permission_denied, got %v", err)
	}
}

func TestWriteCreatesFile(t *testing.T) {
	p, rootfs := makeProxy(t)
	_, err := p.Write(encode(t, rpc.FsWriteParams{Path: "/data/new.txt", Data: []byte("hello")}))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(rootfs, "data", "new.txt"))
	if err != nil || string(b) != "hello" {
		t.Fatalf("file not written: err=%v body=%q", err, b)
	}
}

func TestListEnumeratesEntries(t *testing.T) {
	p, rootfs := makeProxy(t)
	os.MkdirAll(filepath.Join(rootfs, "data", "sub"), 0o755)
	os.WriteFile(filepath.Join(rootfs, "data", "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(rootfs, "data", "b.txt"), []byte("bb"), 0o644)

	got, err := p.List(encode(t, rpc.FsListParams{Path: "/data"}))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	entries := got.(rpc.FsListResult).Entries
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d (%+v)", len(entries), entries)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if !names["a.txt"] || !names["b.txt"] || !names["sub"] {
		t.Fatalf("missing entries: %+v", names)
	}
}

func TestRelativePathRejected(t *testing.T) {
	p, _ := makeProxy(t)
	_, err := p.Read(encode(t, rpc.FsReadParams{Path: "relative/path"}))
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrCodeInvalidParams {
		t.Fatalf("want invalid_params, got %v", err)
	}
}

// TestMountRedirect: an Agent-side path under a mounted volume must
// resolve to the volume's on-disk location, NOT to the Room's rootfs.
// This is the fix for: fsproxy runs outside the Agent's mount ns so
// the bind-mount is invisible to it without an explicit redirect.
func TestMountRedirect(t *testing.T) {
	p, rootfs := makeProxy(t)
	volDir := t.TempDir()
	p.Mounts = []MountRedirect{{AgentPath: "/shared/kb", HostPath: volDir}}

	// Extend Rank's allow-list to include the mountpoint (daemon does
	// this at hire time).
	p.Rank.FSRead = append(p.Rank.FSRead, "/shared/kb")
	p.Rank.FSWrite = append(p.Rank.FSWrite, "/shared/kb")

	_, err := p.Write(encode(t, rpc.FsWriteParams{Path: "/shared/kb/file.txt", Data: []byte("hello")}))
	if err != nil {
		t.Fatal(err)
	}

	// File should land in volDir, NOT in rootfs.
	if _, err := os.Stat(filepath.Join(volDir, "file.txt")); err != nil {
		t.Fatalf("file not at volume location: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "shared", "kb", "file.txt")); err == nil {
		t.Fatal("file leaked into rootfs; mount redirect didn't fire")
	}

	got, err := p.Read(encode(t, rpc.FsReadParams{Path: "/shared/kb/file.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if string(got.(rpc.FsReadResult).Data) != "hello" {
		t.Fatalf("round-trip via mount failed: %v", got)
	}
}

// TestMountRedirect_LongestPrefixWins: when two mounts overlap, the
// longer AgentPath should win the resolve.
func TestMountRedirect_LongestPrefixWins(t *testing.T) {
	p, _ := makeProxy(t)
	outer := t.TempDir()
	inner := t.TempDir()
	p.Mounts = []MountRedirect{
		{AgentPath: "/shared/kb/docs", HostPath: inner},
		{AgentPath: "/shared/kb", HostPath: outer},
	}
	p.Rank.FSRead = append(p.Rank.FSRead, "/shared/kb")
	p.Rank.FSWrite = append(p.Rank.FSWrite, "/shared/kb")

	_, err := p.Write(encode(t, rpc.FsWriteParams{Path: "/shared/kb/docs/paper.pdf", Data: []byte("x")}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(inner, "paper.pdf")); err != nil {
		t.Fatalf("inner mount should have received the file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outer, "docs", "paper.pdf")); err == nil {
		t.Fatal("outer mount shouldn't have received a path with a longer inner match")
	}
}

// TestCrossRoomIsolation: two Proxies with different rootfs must not see
// each other's files even if their allow-lists overlap.
func TestCrossRoomIsolation(t *testing.T) {
	pA, rootA := makeProxy(t)
	pB, rootB := makeProxy(t)
	os.WriteFile(filepath.Join(rootA, "data", "secret"), []byte("roomA"), 0o644)
	os.WriteFile(filepath.Join(rootB, "data", "secret"), []byte("roomB"), 0o644)

	gotA, err := pA.Read(encode(t, rpc.FsReadParams{Path: "/data/secret"}))
	if err != nil {
		t.Fatalf("A read: %v", err)
	}
	if string(gotA.(rpc.FsReadResult).Data) != "roomA" {
		t.Fatalf("A got wrong data: %s", gotA.(rpc.FsReadResult).Data)
	}

	gotB, err := pB.Read(encode(t, rpc.FsReadParams{Path: "/data/secret"}))
	if err != nil {
		t.Fatalf("B read: %v", err)
	}
	if string(gotB.(rpc.FsReadResult).Data) != "roomB" {
		t.Fatalf("B got wrong data: %s", gotB.(rpc.FsReadResult).Data)
	}
}
