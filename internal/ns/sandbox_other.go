//go:build !linux

package ns

import (
	"os/exec"
	"path/filepath"
)

// On non-Linux, namespaces aren't available. The demo targets Linux only
// (see DEMO_PLAN.md), but keeping a compile-clean stub lets developers run
// `go vet` / editor tooling on macOS without errors.

// Mount mirrors the Linux-side type so callers compile cleanly.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool
}

func NewAgentCommand(rootfs, imageDir, relEntry string, extraMounts []Mount) (*exec.Cmd, error) {
	return exec.Command(filepath.Join(imageDir, relEntry)), nil
}

func IsInitMode() bool { return false }
func RunInit()         {}
