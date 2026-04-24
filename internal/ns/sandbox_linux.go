//go:build linux

// Package ns configures Linux-namespace sandboxing for Agent subprocesses.
//
// Architecture: we follow the runc pattern of a self re-exec init helper.
// When the daemon wants to spawn an Agent, it doesn't exec the Agent
// directly — it execs /proc/self/exe (i.e. hived) with a reserved sentinel
// first arg. The child process, recognising the sentinel, does all mount
// namespace setup (pivot_root, bind mounts, tmpfs) and then exec's the
// real Agent binary. This lets us configure the namespace BEFORE the Agent
// sees any filesystem.
//
// Why re-exec? Go can't run arbitrary code between fork and exec (no
// PreExec hook — exec.Cmd uses a locked syscall path). Re-exec is the
// idiomatic workaround used by runc, firecracker, gvisor, etc.
package ns

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Mount is an extra bind mount requested by the caller (typically one
// entry per Hivefile `volumes:` item). Source is a host path; Target is
// an absolute path inside the sandbox. ReadOnly adds MS_RDONLY on remount.
type Mount struct {
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"ro,omitempty"`
}

// initSentinel is the reserved first arg recognised by RunInit.
// Chosen to be unlikely to collide with any legitimate subcommand.
const initSentinel = "__hive_init__"

// EnvNoSandbox, when set to "1", disables all namespace work. Useful for
// running tests on non-root CI or for cross-platform development.
const EnvNoSandbox = "HIVE_NO_SANDBOX"

// NewAgentCommand returns an exec.Cmd that, when Start()ed, spawns the
// Agent inside a private mount+network namespace rooted at rootfs.
// imageDir is bind-mounted read-only at /app inside the sandbox; relEntry
// is the path to the Agent binary relative to imageDir.
//
// extraMounts are additional bind mounts the init helper will set up
// (beneath rootfs, before pivot_root). Typically one per Hivefile
// `volumes:` entry. Nil/empty means no extra mounts.
//
// The second return value is the read end of an init-error pipe: RunInit
// writes any setup failure to FD 3 (the corresponding write end) before
// exit; on success it closes FD 3 right before syscall.Exec so the real
// Agent binary never sees it. Callers read this pipe to get a real error
// message instead of a generic "exit status 1". Returned pipe is nil when
// sandboxing is disabled (HIVE_NO_SANDBOX=1).
func NewAgentCommand(rootfs, imageDir, relEntry string, extraMounts []Mount) (*exec.Cmd, *os.File, error) {
	if os.Getenv(EnvNoSandbox) == "1" {
		return exec.Command(filepath.Join(imageDir, relEntry)), nil, nil
	}
	self, err := os.Executable()
	if err != nil {
		return nil, nil, fmt.Errorf("resolve self: %w", err)
	}
	argv := []string{self, initSentinel, rootfs, imageDir, relEntry}
	if len(extraMounts) > 0 {
		b, err := json.Marshal(extraMounts)
		if err != nil {
			return nil, nil, fmt.Errorf("encode extra mounts: %w", err)
		}
		argv = append(argv, string(b))
	}
	initErrR, initErrW, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("init-err pipe: %w", err)
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWNS | syscall.CLONE_NEWNET,
	}
	// ExtraFiles[0] → child FD 3. Caller (agent.Conn.Start) closes the
	// parent's copy after cmd.Start so EOF propagates when the child exits.
	cmd.ExtraFiles = []*os.File{initErrW}
	return cmd, initErrR, nil
}

// IsInitMode reports whether the current process was re-execed by
// NewAgentCommand to act as the namespace init helper.
func IsInitMode() bool {
	return len(os.Args) > 1 && os.Args[1] == initSentinel
}

// RunInit performs namespace setup and exec's the Agent. Called from
// hived's main() when IsInitMode() returns true. Never returns on success
// (calls syscall.Exec); calls os.Exit(1) on failure.
//
// A copy of every failure is also written to FD 3 — the parent attaches
// a pipe there via NewAgentCommand so it can surface a real error string
// through the AgentHire RPC instead of a generic "exit status 1".
func RunInit() {
	// FD 3 is the init-err pipe set up by the parent via cmd.ExtraFiles.
	// Nil only in the (unlikely) case it was never attached — treat all
	// writes as best-effort so the stderr fallback remains the source of
	// truth for log-file diagnostics.
	initErr := os.NewFile(3, "hive-init-err")
	fail := func(format string, args ...any) {
		fmt.Fprintf(os.Stderr, "hive-init "+format, args...)
		if initErr != nil {
			fmt.Fprintf(initErr, format, args...)
			_ = initErr.Close()
		}
		os.Exit(1)
	}

	if len(os.Args) < 5 {
		fail("expected [sentinel rootfs imageDir relEntry]\n")
	}
	rootfs := os.Args[2]
	imageDir := os.Args[3]
	relEntry := os.Args[4]
	var extraMounts []Mount
	if len(os.Args) >= 6 {
		if err := json.Unmarshal([]byte(os.Args[5]), &extraMounts); err != nil {
			fail("parse extra mounts: %v\n", err)
		}
	}

	if err := setupSandbox(rootfs, imageDir, extraMounts); err != nil {
		fail("setup: %v\n", err)
	}

	// Sandbox is ready. Close the init-err pipe so the parent's read side
	// sees EOF with zero bytes — that's how WaitInit distinguishes success
	// from failure. Must happen before syscall.Exec or the real Agent
	// would inherit FD 3.
	if initErr != nil {
		_ = initErr.Close()
	}

	entry := filepath.Join("/app", relEntry)
	argv := []string{entry}
	if err := syscall.Exec(entry, argv, os.Environ()); err != nil {
		// initErr already closed — exec failure is rare (path bind-mounted,
		// perms validated) and the stderr log still captures it.
		fmt.Fprintf(os.Stderr, "hive-init exec %s: %v\n", entry, err)
		os.Exit(1)
	}
}

// setupSandbox runs inside the cloned child. After success, the caller's
// mount view is rooted at rootfs, and the network namespace is empty
// except for the kernel's default state (no interfaces, not even loopback
// is up — that's intentional, it forces all network I/O through Hive's
// proxy). The Agent binary is reachable at /app/<relEntry>.
func setupSandbox(rootfs, imageDir string, extraMounts []Mount) error {
	// 1. Make the mount tree private so our work doesn't leak to the host.
	//    Even though CLONE_NEWNS gives us a copy, shared subtrees would
	//    still propagate; MS_PRIVATE severs that.
	if err := syscall.Mount("", "/", "", syscall.MS_REC|syscall.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}

	// 2. pivot_root requires the new root to be a mount point. Bind rootfs
	//    onto itself to satisfy that constraint.
	if err := syscall.Mount(rootfs, rootfs, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind rootfs: %w", err)
	}

	// 3. Bind the Image dir into <rootfs>/app (read-only) so we can exec
	//    the Agent binary after pivot_root.
	appDir := filepath.Join(rootfs, "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		return fmt.Errorf("mkdir /app: %w", err)
	}
	if err := bindReadOnly(imageDir, appDir); err != nil {
		return err
	}

	// 4. Bind shared system dirs read-only. Go binaries are usually
	//    statically linked, so most of these are cosmetic, but /etc
	//    matters for DNS resolution (when we want it) and /usr/lib/...
	//    covers dynamically linked tools.
	for _, dir := range []string{"/usr", "/lib", "/lib64", "/bin", "/sbin", "/etc"} {
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		dst := filepath.Join(rootfs, dir)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dst, err)
		}
		if err := bindReadOnly(dir, dst); err != nil {
			return err
		}
	}

	// 5. Writable Room-private mounts:
	//    - /tmp: tmpfs, erased on Agent exit
	//    - /data: on-disk dir under the Room's rootfs (persisted)
	tmpDir := filepath.Join(rootfs, "tmp")
	if err := os.MkdirAll(tmpDir, 0o1777); err != nil {
		return fmt.Errorf("mkdir /tmp: %w", err)
	}
	if err := syscall.Mount("tmpfs", tmpDir, "tmpfs", 0, "mode=1777"); err != nil {
		return fmt.Errorf("mount tmpfs /tmp: %w", err)
	}
	dataDir := filepath.Join(rootfs, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return fmt.Errorf("mkdir /data: %w", err)
	}

	// 6. /proc is needed by Go runtime internals (signal handling, etc.).
	procDir := filepath.Join(rootfs, "proc")
	if err := os.MkdirAll(procDir, 0o555); err != nil {
		return fmt.Errorf("mkdir /proc: %w", err)
	}
	if err := syscall.Mount("proc", procDir, "proc", 0, ""); err != nil {
		return fmt.Errorf("mount proc: %w", err)
	}

	// 7. Extra bind mounts (typically one per Hivefile volume entry).
	//    Done BEFORE pivot_root so the host source paths are still
	//    reachable. Target is an absolute path inside the sandbox
	//    (e.g. /shared/kb), which we translate to <rootfs>/<target>.
	for _, m := range extraMounts {
		if m.Source == "" || m.Target == "" {
			return fmt.Errorf("extra mount: source and target are required (%+v)", m)
		}
		if m.Target[0] != '/' {
			return fmt.Errorf("extra mount target must be absolute: %q", m.Target)
		}
		dst := filepath.Join(rootfs, m.Target)
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dst, err)
		}
		if err := syscall.Mount(m.Source, dst, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
			return fmt.Errorf("bind %s → %s: %w", m.Source, dst, err)
		}
		if m.ReadOnly {
			// Same remount-ro trick as the /usr bind mounts above —
			// MS_BIND|MS_REMOUNT|MS_RDONLY in a second Mount call.
			if err := syscall.Mount("", dst, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
				return fmt.Errorf("remount ro %s: %w", dst, err)
			}
		}
	}

	// 8. pivot_root: new_root = rootfs, put_old = rootfs/.pivot_root.
	pivotOld := filepath.Join(rootfs, ".pivot_root")
	if err := os.MkdirAll(pivotOld, 0o700); err != nil {
		return fmt.Errorf("mkdir pivot_old: %w", err)
	}
	if err := syscall.PivotRoot(rootfs, pivotOld); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return fmt.Errorf("chdir /: %w", err)
	}
	// Detach old root; its mounts will be freed once all FDs referencing
	// them are closed (Agent gets a clean view immediately).
	if err := syscall.Unmount("/.pivot_root", syscall.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	if err := os.Remove("/.pivot_root"); err != nil {
		return fmt.Errorf("remove /.pivot_root: %w", err)
	}
	return nil
}

func bindReadOnly(src, dst string) error {
	if err := syscall.Mount(src, dst, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil {
		return fmt.Errorf("bind %s → %s: %w", src, dst, err)
	}
	// Second mount call remounts with the ro flag — a single Mount(bind|ro)
	// doesn't set ro because bind ignores the flags other than MS_BIND.
	if err := syscall.Mount("", dst, "", syscall.MS_BIND|syscall.MS_REMOUNT|syscall.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("remount ro %s: %w", dst, err)
	}
	return nil
}
