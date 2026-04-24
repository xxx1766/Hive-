// Package aitoolproxy handles Agent ai_tool/invoke requests. This is the
// Hive-side adapter for "CLI-shaped AI tools" — specifically Claude Code
// CLI, with room for Cursor / Codex / any future vendor that exposes a
// cwd-based one-shot interface.
//
// The tool runs as a child process of hived (host namespace), with its
// cwd pinned to the calling Room's workspace dir. Output is captured and
// returned to the Agent. The Agent never sees the host-side path; from
// its perspective it fs_write's into /workspace and then calls
// ai_tool/invoke — claude-code finds the files waiting there.
//
// Security boundary (intentionally soft in MVP):
//   - cwd pinning limits what claude's default tools touch
//   - Rank.AIToolAllowed + per-Agent quota are hard gates
//   - claude's Bash tool is NOT sandboxed here; hard confinement
//     (firejail / bwrap / user namespace) is v2.
package aitoolproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/quota"
	"github.com/anne-x/hive/internal/rank"
	"github.com/anne-x/hive/internal/rpc"
)

// DefaultTimeout caps a single ai_tool/invoke call. 5 minutes is enough
// for typical Claude Code refactor tasks; longer tasks should be split.
const DefaultTimeout = 5 * time.Minute

// Result is the raw output of an ai-tool invocation — Provider-agnostic.
type Result struct {
	Output   string
	Stderr   string
	ExitCode int
}

// Provider is the pluggable backend: one per CLI tool. MVP ships
// ClaudeCodeProvider + MockProvider (used when no API key / binary).
type Provider interface {
	Name() string                                                         // "claude-code"
	Invoke(ctx context.Context, cwd, prompt string, timeout time.Duration) (Result, error)
}

// Proxy is built per-Agent.
type Proxy struct {
	RoomID    string
	AgentName string
	Rank      *rank.Rank
	Quota     *quota.Actor
	Provider  Provider // may be nil ⇒ reject with "no ai-tool Provider configured"
	Workspace string   // absolute host-side path to the Room's workspace dir
}

// Invoke is the ai_tool/invoke handler.
func (p *Proxy) Invoke(ctx context.Context, params json.RawMessage) (any, error) {
	var ip rpc.AIToolInvokeParams
	if err := json.Unmarshal(params, &ip); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	if !p.Rank.AIToolAllowed {
		return nil, protocol.ErrPermissionDenied("rank " + p.Rank.Name + " cannot call ai_tool/invoke")
	}
	if p.Provider == nil {
		return nil, protocol.NewError(protocol.ErrCodeInternal,
			"no ai-tool Provider configured (set ANTHROPIC_API_KEY + install `claude` CLI on PATH)")
	}
	if ip.Tool == "" {
		ip.Tool = p.Provider.Name()
	}
	if ip.Tool != p.Provider.Name() {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams,
			fmt.Sprintf("ai_tool %q not available (provider is %q)", ip.Tool, p.Provider.Name()))
	}

	// Quota is charged once per invocation (regardless of input size).
	qkey := quota.Key{
		RoomID:   p.RoomID,
		Agent:    p.AgentName,
		Resource: "ai_tool:" + p.Provider.Name(),
	}
	qr, qerr := p.Quota.Consume(ctx, qkey, 1)
	if qerr != nil {
		return nil, qerr
	}
	if !qr.Allowed {
		return nil, protocol.ErrQuotaExceeded(fmt.Sprintf(
			"ai_tool:%s quota exhausted (remaining=%d)", p.Provider.Name(), qr.Remaining))
	}

	timeout := DefaultTimeout
	if ip.Timeout > 0 {
		timeout = time.Duration(ip.Timeout) * time.Second
	}

	res, err := p.Provider.Invoke(ctx, p.Workspace, ip.Prompt, timeout)
	if err != nil {
		return nil, fmt.Errorf("ai_tool/invoke: %w", err)
	}
	return rpc.AIToolInvokeResult{
		Output:   res.Output,
		Stderr:   res.Stderr,
		ExitCode: res.ExitCode,
	}, nil
}

// ── ClaudeCodeProvider ────────────────────────────────────────────────────

// ClaudeCodeProvider execs the `claude` CLI. Requires ANTHROPIC_API_KEY in
// the daemon's environment (passed through to the child).
type ClaudeCodeProvider struct {
	Binary string // path to `claude`; empty ⇒ "claude" resolved via PATH
}

// NewClaudeCodeFromEnv returns a Provider iff:
//   - ANTHROPIC_API_KEY is set, AND
//   - `claude` (or the override in CLAUDE_CODE_BIN) is executable on PATH.
//
// Returns nil if either condition fails so the daemon can fall back to
// MockProvider with a clear message.
func NewClaudeCodeFromEnv() *ClaudeCodeProvider {
	if os.Getenv("ANTHROPIC_API_KEY") == "" {
		return nil
	}
	bin := os.Getenv("CLAUDE_CODE_BIN")
	if bin == "" {
		bin = "claude"
	}
	if _, err := exec.LookPath(bin); err != nil {
		return nil
	}
	return &ClaudeCodeProvider{Binary: bin}
}

func (c *ClaudeCodeProvider) Name() string { return "claude-code" }

func (c *ClaudeCodeProvider) Invoke(ctx context.Context, cwd, prompt string, timeout time.Duration) (Result, error) {
	if cwd == "" {
		return Result{}, errors.New("claude-code: cwd (workspace) is required")
	}
	// Ensure the workspace exists; without it claude fails with an
	// unfriendly "no such file or directory" from exec.
	if fi, err := os.Stat(cwd); err != nil {
		return Result{}, fmt.Errorf("workspace %q: %w", cwd, err)
	} else if !fi.IsDir() {
		return Result{}, fmt.Errorf("workspace %q is not a directory", cwd)
	}

	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// -p: print mode (non-interactive); stdin empty.
	//     (Keeping it conservative — the user can layer --allowed-tools /
	//      --permission-mode later through an opts bag.)
	cmd := exec.CommandContext(tctx, c.Binary, "-p", prompt)
	cmd.Dir = cwd
	// Pass through the daemon's env so ANTHROPIC_API_KEY and proxy vars
	// (HTTPS_PROXY, etc.) are visible to claude.
	cmd.Env = os.Environ()

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	err := cmd.Run()
	res := Result{
		Output: outBuf.String(),
		Stderr: errBuf.String(),
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
		// Non-zero exit isn't a daemon-level error — surface it so the
		// Agent can decide what to do. Only propagate err if the process
		// itself failed to start or got context-cancelled.
		return res, nil
	}
	if errors.Is(tctx.Err(), context.DeadlineExceeded) {
		return res, fmt.Errorf("claude-code timed out after %s", timeout)
	}
	if err != nil {
		return res, fmt.Errorf("exec claude: %w", err)
	}
	return res, nil
}

// ── MockProvider ──────────────────────────────────────────────────────────

// MockProvider is the offline-safe default. It writes a trace line to the
// workspace so integration tests can verify that cwd pinning worked.
type MockProvider struct{}

func (MockProvider) Name() string { return "claude-code" }

func (MockProvider) Invoke(ctx context.Context, cwd, prompt string, _ time.Duration) (Result, error) {
	// Leave a breadcrumb in the workspace — lets tests / demo assert
	// "the mock was invoked in the expected cwd".
	if cwd != "" {
		if fi, err := os.Stat(cwd); err == nil && fi.IsDir() {
			_ = os.WriteFile(cwd+"/.hive-ai-tool-mock.log",
				[]byte("mock invocation at "+time.Now().Format(time.RFC3339)+"\nprompt: "+prompt+"\n"),
				0o640)
		}
	}
	return Result{
		Output:   "mock-claude: " + truncate(prompt, 500),
		ExitCode: 0,
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.TrimSpace(s[:n]) + "…"
}
