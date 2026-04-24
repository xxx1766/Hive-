package ipc

import (
	"os"
	"path/filepath"
)

// SocketPath returns the Unix socket location for daemon↔CLI IPC.
// Priority: $HIVE_SOCKET → $XDG_RUNTIME_DIR/hive/hived.sock → ~/.hive/hived.sock.
func SocketPath() string {
	if s := os.Getenv("HIVE_SOCKET"); s != "" {
		return s
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "hive", "hived.sock")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	return filepath.Join(home, ".hive", "hived.sock")
}

// StateRoot is the daemon's persistent state directory.
// Priority: $HIVE_STATE → ~/.hive.
func StateRoot() string {
	if s := os.Getenv("HIVE_STATE"); s != "" {
		return s
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "/tmp"
	}
	return filepath.Join(home, ".hive")
}

func ImagesDir() string  { return filepath.Join(StateRoot(), "images") }
func RoomsDir() string   { return filepath.Join(StateRoot(), "rooms") }
func VolumesDir() string { return filepath.Join(StateRoot(), "volumes") }

// WorkspaceDir is the Room's scratch dir — bind-mounted into every
// Agent's sandbox at /workspace, and used as the cwd for ai_tool/invoke
// (so Claude Code's default file operations stay scoped to this dir).
func WorkspaceDir(roomID string) string {
	return filepath.Join(RoomsDir(), roomID, "workspace")
}
