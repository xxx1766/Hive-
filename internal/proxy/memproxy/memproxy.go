// Package memproxy handles Agent memory/* requests — a structured KV API
// layered on top of the Volume subsystem.
//
// Scope resolution:
//
//	""        → Room-private: <RoomsDir>/<roomID>/memory/
//	"<name>"  → Volume-shared: <VolumeRoot>/<name>/memory/ (must exist)
//
// Backend: one file per key. filename = url.PathEscape(key). Reads are
// a straight os.ReadFile; writes go through a .tmp + rename for atomicity
// (so readers never observe a half-written value). List walks the dir and
// url-decodes filenames to reconstruct keys.
//
// Why file-per-key instead of SQLite / BoltDB:
//   - no new module dependencies (aligns with the rest of Hive)
//   - values stay cat-readable, helping debugging
//   - "几轮才有要记的要点" — low write rate, dozens of keys per volume,
//     so per-file overhead is irrelevant
//
// Access control:
//   - Rank.MemoryAllowed must be true for ANY memory/* call
//   - Any (Room, Agent) with MemoryAllowed can read/write any scope.
//     Row-level / volume-level ACL is deferred until a use case demands it.
package memproxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/rank"
	"github.com/anne-x/hive/internal/rpc"
	"github.com/anne-x/hive/internal/volume"
)

// MaxKeyLen bounds filenames so pathological keys don't DOS the FS.
const MaxKeyLen = 256

// Proxy is constructed per-Agent. RoomID + AgentName are used for
// logging and for resolving private scope; Volumes is the daemon-wide
// manager for shared scope.
type Proxy struct {
	RoomID    string
	AgentName string
	Rank      *rank.Rank
	Volumes   *volume.Manager
	RoomsDir  string // ~/.hive/rooms (for private scope)
}

// ── Handlers ─────────────────────────────────────────────────────────────

func (p *Proxy) Put(params json.RawMessage) (any, error) {
	if err := p.gate(); err != nil {
		return nil, err
	}
	var r rpc.MemoryPutParams
	if err := json.Unmarshal(params, &r); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	if err := validateKey(r.Key); err != nil {
		return nil, err
	}
	dir, err := p.resolveDir(r.Scope)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, encodeKey(r.Key))
	// Atomic write: staging file + rename. Readers never see partial data.
	tmp, err := os.CreateTemp(dir, ".put-*")
	if err != nil {
		return nil, err
	}
	if _, err := tmp.Write(r.Value); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return nil, err
	}
	return struct{}{}, nil
}

func (p *Proxy) Get(params json.RawMessage) (any, error) {
	if err := p.gate(); err != nil {
		return nil, err
	}
	var r rpc.MemoryGetParams
	if err := json.Unmarshal(params, &r); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	if err := validateKey(r.Key); err != nil {
		return nil, err
	}
	dir, err := p.resolveDir(r.Scope)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, encodeKey(r.Key))
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rpc.MemoryGetResult{Exists: false}, nil
		}
		return nil, err
	}
	return rpc.MemoryGetResult{Value: data, Exists: true}, nil
}

func (p *Proxy) List(params json.RawMessage) (any, error) {
	if err := p.gate(); err != nil {
		return nil, err
	}
	var r rpc.MemoryListParams
	if err := json.Unmarshal(params, &r); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	dir, err := p.resolveDir(r.Scope)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return rpc.MemoryListResult{Keys: []string{}}, nil
		}
		return nil, err
	}
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".put-") {
			continue
		}
		k, err := decodeKey(e.Name())
		if err != nil {
			continue // skip unrecognised filenames
		}
		if r.Prefix != "" && !strings.HasPrefix(k, r.Prefix) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return rpc.MemoryListResult{Keys: keys}, nil
}

func (p *Proxy) Delete(params json.RawMessage) (any, error) {
	if err := p.gate(); err != nil {
		return nil, err
	}
	var r rpc.MemoryDeleteParams
	if err := json.Unmarshal(params, &r); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	if err := validateKey(r.Key); err != nil {
		return nil, err
	}
	dir, err := p.resolveDir(r.Scope)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, encodeKey(r.Key))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return struct{}{}, nil
}

// ── helpers ─────────────────────────────────────────────────────────────

// gate is the single Rank check for every memory/* handler. Memory is
// binary-gated: a Rank either has it or it doesn't. ACL on the scope
// itself is future work.
func (p *Proxy) gate() error {
	if !p.Rank.MemoryAllowed {
		return protocol.ErrPermissionDenied("rank " + p.Rank.Name + " cannot use memory/*")
	}
	return nil
}

// resolveDir turns a scope string into the on-disk memory/ directory.
func (p *Proxy) resolveDir(scope string) (string, error) {
	if scope == "" {
		return filepath.Join(p.RoomsDir, p.RoomID, "memory"), nil
	}
	vol, err := p.Volumes.Get(scope)
	if err != nil {
		return "", protocol.NewError(protocol.ErrCodeInvalidParams,
			fmt.Sprintf("memory: scope %q: %v — create with `hive volume create %s`", scope, err, scope))
	}
	return vol.MemoryDir(), nil
}

func validateKey(k string) error {
	if k == "" {
		return protocol.NewError(protocol.ErrCodeInvalidParams, "memory: key must not be empty")
	}
	if len(k) > MaxKeyLen {
		return protocol.NewError(protocol.ErrCodeInvalidParams,
			fmt.Sprintf("memory: key too long (%d > %d)", len(k), MaxKeyLen))
	}
	if !utf8.ValidString(k) {
		return protocol.NewError(protocol.ErrCodeInvalidParams, "memory: key must be valid UTF-8")
	}
	for _, r := range k {
		if r == 0 {
			return protocol.NewError(protocol.ErrCodeInvalidParams, "memory: key must not contain NUL")
		}
	}
	return nil
}

// encodeKey makes a key safe to use as a filename.
// url.PathEscape covers /, %, control chars. We also swap "." to avoid
// dot-collision with our .put-*.tmp sentinel — belt-and-suspenders since
// the sentinel prefix also filters it.
func encodeKey(k string) string {
	return url.PathEscape(k)
}

func decodeKey(name string) (string, error) {
	return url.PathUnescape(name)
}
