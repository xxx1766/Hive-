package daemon

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anne-x/hive/internal/conversation"
	"github.com/anne-x/hive/internal/room"
)

// outputScanCap bounds the per-conversation Outputs list so a runaway
// workflow that touches thousands of files doesn't bloat the on-disk
// conversation JSON.
const outputScanCap = 200

// outputScanSubdirs are the only paths walked when collecting Outputs.
// Restricting the scan to "where Agents write" keeps user-uploaded
// fixtures (which already live next to memory/ via the /api/volumes
// upload endpoint) out of the result — the upload endpoint sets mtime
// to "now" but the user clearly didn't author that as task output.
var outputScanSubdirs = []string{"memory", "uploads"}

// collectConversationOutputs walks the Volumes mounted into any of the
// Room's Members and returns files whose mtime is inside the
// conversation's [StartedAt, until] window. Cap at outputScanCap.
//
// Best-effort attribution caveat: mtime is shared filesystem state;
// when two convs run concurrently in the same Room, each will see the
// other's writes. Documented in conversation.OutputRef. Fixing this
// properly needs conv_id propagation through memproxy.Put — out of
// scope this round.
func (d *Daemon) collectConversationOutputs(r *room.Room, c *conversation.Conversation, until time.Time) []conversation.OutputRef {
	if r == nil || c == nil || c.StartedAt.IsZero() {
		return nil
	}
	// Distinct volume names mounted into any Member of this Room.
	seen := map[string]bool{}
	var names []string
	for _, m := range r.Members() {
		for _, v := range m.Volumes {
			if !seen[v.Name] {
				seen[v.Name] = true
				names = append(names, v.Name)
			}
		}
	}
	if len(names) == 0 {
		return nil
	}
	// StartedAt filtering is inclusive (>=) so we don't miss writes that
	// land in the same nanosecond the conv flipped to active. Subtract
	// one second of slack to absorb fs mtime granularity (some kernels
	// truncate to 1s on certain mounts).
	since := c.StartedAt.Add(-time.Second)

	var out []conversation.OutputRef
	for _, name := range names {
		vol, err := d.volumes.Get(name)
		if err != nil {
			continue
		}
		for _, sub := range outputScanSubdirs {
			root := filepath.Join(vol.Path, sub)
			if _, err := os.Stat(root); err != nil {
				continue
			}
			if len(out) >= outputScanCap {
				break
			}
			out = walkOutputs(root, vol.Path, vol.Name, since, until, out)
		}
		if len(out) >= outputScanCap {
			break
		}
	}
	// Most recent first — user usually wants the freshest output.
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	if len(out) > outputScanCap {
		out = out[:outputScanCap]
	}
	return out
}

// walkOutputs is a recursive directory walker that skips hidden files,
// stops at outputScanCap, and appends matching entries to acc.
func walkOutputs(dir, volRoot, volName string, since, until time.Time, acc []conversation.OutputRef) []conversation.OutputRef {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return acc
	}
	for _, e := range entries {
		if len(acc) >= outputScanCap {
			return acc
		}
		nm := e.Name()
		if strings.HasPrefix(nm, ".") {
			continue // dotfiles + atomic-write tempfiles
		}
		full := filepath.Join(dir, nm)
		if e.IsDir() {
			acc = walkOutputs(full, volRoot, volName, since, until, acc)
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		mt := info.ModTime()
		if mt.Before(since) || mt.After(until) {
			continue
		}
		// Path relative to the volume root, posix-style — same shape as
		// the volume tree endpoint returns, so the UI can treat them
		// interchangeably for click-through.
		rel, err := filepath.Rel(volRoot, full)
		if err != nil {
			continue
		}
		acc = append(acc, conversation.OutputRef{
			Volume:  volName,
			Path:    filepath.ToSlash(rel),
			Size:    info.Size(),
			ModTime: mt.UTC(),
		})
	}
	return acc
}
