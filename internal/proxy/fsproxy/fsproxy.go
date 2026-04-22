// Package fsproxy handles Agent→Hive fs/* requests. It consults the Agent's
// Rank to allow/deny, then performs the I/O from the daemon's viewpoint.
//
// Path translation: the Agent sees its Room's rootfs as /. The daemon runs
// in the host namespace, so an Agent path like "/data/paper.pdf" is served
// from <room.Rootfs>/data/paper.pdf on disk. This is also where cross-Room
// isolation is enforced: by translating through the Room's rootfs, there
// is no filename an Agent can spell that reaches another Room.
package fsproxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/rank"
	"github.com/anne-x/hive/internal/rpc"
)

// Proxy wraps the per-Agent context the handlers need.
type Proxy struct {
	RoomRootfs string // absolute path on host
	Rank       *rank.Rank
}

// Read reads a file at Agent-perspective path.
func (p *Proxy) Read(params json.RawMessage) (any, error) {
	var rp rpc.FsReadParams
	if err := json.Unmarshal(params, &rp); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	host, err := p.resolve(rp.Path)
	if err != nil {
		return nil, err
	}
	if !p.Rank.AllowRead(rp.Path) {
		return nil, protocol.ErrPermissionDenied("rank " + p.Rank.Name + " cannot read " + rp.Path)
	}
	data, err := os.ReadFile(host)
	if err != nil {
		return nil, fmt.Errorf("fs/read %s: %w", rp.Path, err)
	}
	return rpc.FsReadResult{Data: data}, nil
}

// Write creates or overwrites a file at Agent-perspective path.
func (p *Proxy) Write(params json.RawMessage) (any, error) {
	var wp rpc.FsWriteParams
	if err := json.Unmarshal(params, &wp); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	host, err := p.resolve(wp.Path)
	if err != nil {
		return nil, err
	}
	if !p.Rank.AllowWrite(wp.Path) {
		return nil, protocol.ErrPermissionDenied("rank " + p.Rank.Name + " cannot write " + wp.Path)
	}
	if err := os.MkdirAll(filepath.Dir(host), 0o755); err != nil {
		return nil, fmt.Errorf("fs/write mkdir: %w", err)
	}
	if err := os.WriteFile(host, wp.Data, 0o640); err != nil {
		return nil, fmt.Errorf("fs/write: %w", err)
	}
	return struct{}{}, nil
}

// List enumerates a directory at Agent-perspective path.
func (p *Proxy) List(params json.RawMessage) (any, error) {
	var lp rpc.FsListParams
	if err := json.Unmarshal(params, &lp); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	host, err := p.resolve(lp.Path)
	if err != nil {
		return nil, err
	}
	if !p.Rank.AllowRead(lp.Path) {
		return nil, protocol.ErrPermissionDenied("rank " + p.Rank.Name + " cannot list " + lp.Path)
	}
	entries, err := os.ReadDir(host)
	if err != nil {
		return nil, fmt.Errorf("fs/list %s: %w", lp.Path, err)
	}
	out := make([]rpc.FsEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		var size int64
		if err == nil && !e.IsDir() {
			size = info.Size()
		}
		out = append(out, rpc.FsEntry{
			Name:  e.Name(),
			IsDir: e.IsDir(),
			Size:  size,
		})
	}
	return rpc.FsListResult{Entries: out}, nil
}

// resolve turns an Agent-visible absolute path into a host-side absolute path.
// Enforces: path must be absolute, must not escape rootfs via '..'.
func (p *Proxy) resolve(agentPath string) (string, error) {
	if !strings.HasPrefix(agentPath, "/") {
		return "", protocol.NewError(protocol.ErrCodeInvalidParams, "path must be absolute: "+agentPath)
	}
	cleaned := filepath.Clean(agentPath)
	// After Clean, "/../x" becomes "/x", so prefix-based containment is safe.
	host := filepath.Join(p.RoomRootfs, cleaned)
	// Defense-in-depth: verify host still starts with rootfs.
	if !strings.HasPrefix(host, p.RoomRootfs+string(filepath.Separator)) && host != p.RoomRootfs {
		return "", protocol.NewError(protocol.ErrCodeInvalidParams, "path escapes rootfs: "+agentPath)
	}
	return host, nil
}
