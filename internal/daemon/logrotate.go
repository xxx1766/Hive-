package daemon

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
)

// Per-Agent stderr log budget. Motivation (see README TODO): without a cap
// the file grows forever. We don't need deep archaeology — daemon.log
// already carries room-scoped context — so one rotated backup is enough
// ("don't fill the disk" beats "keep every byte"). Override with
// HIVE_LOG_MAX_BYTES for demos that want tighter or looser bounds.
const (
	defaultLogMaxBytes = 10 * 1024 * 1024 // 10 MiB per Agent
	logMaxBytesEnv     = "HIVE_LOG_MAX_BYTES"
)

func logMaxBytes() int64 {
	if s := os.Getenv(logMaxBytesEnv); s != "" {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultLogMaxBytes
}

// rotatingLog is an io.WriteCloser that caps a single stderr log file.
// When a pending write would push it over maxBytes, the current file is
// renamed to <path>.1 (displacing any previous .1) and a fresh file is
// opened. Exactly one rotated backup is kept.
//
// Writes come from os/exec's internal copy goroutine (which Hive gets for
// free whenever cmd.Stderr is not a concrete *os.File), so concurrent
// writers are not a concern — the mutex only serialises a rotation
// against a write that started just before it.
type rotatingLog struct {
	path     string
	maxBytes int64

	mu   sync.Mutex
	f    *os.File
	size int64
}

var _ io.WriteCloser = (*rotatingLog)(nil)

func openRotatingLog(path string, maxBytes int64) (*rotatingLog, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		return nil, err
	}
	var size int64
	if fi, err := f.Stat(); err == nil {
		size = fi.Size()
	}
	return &rotatingLog{path: path, maxBytes: maxBytes, f: f, size: size}, nil
}

func (r *rotatingLog) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return 0, errors.New("rotating log closed")
	}
	// Guard with size > 0 so a single oversized first write still lands
	// (pathological but harmless — it rotates on the next write).
	if r.size > 0 && r.size+int64(len(p)) > r.maxBytes {
		// Rotation failure is best-effort: we keep writing to the current
		// file rather than drop diagnostics. The log just overshoots the
		// cap for one write; next attempt retries the rotate.
		_ = r.rotateLocked()
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotatingLog) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f == nil {
		return nil
	}
	f := r.f
	r.f = nil
	return f.Close()
}

// rotateLocked performs the close → remove old .1 → rename current → reopen
// dance. On rename failure it reopens the original path so subsequent
// writes still land — losing the rotation but preserving write liveness.
func (r *rotatingLog) rotateLocked() error {
	if err := r.f.Close(); err != nil {
		return fmt.Errorf("close for rotate: %w", err)
	}
	old := r.path + ".1"
	_ = os.Remove(old) // ignore ENOENT
	if err := os.Rename(r.path, old); err != nil {
		// Rename failed (e.g. different fs, race). Reopen the current
		// file so we don't strand the writer on a closed FD.
		if f, e := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640); e == nil {
			r.f = f
		}
		return fmt.Errorf("rename to .1: %w", err)
	}
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return fmt.Errorf("reopen after rotate: %w", err)
	}
	r.f = f
	r.size = 0
	return nil
}
