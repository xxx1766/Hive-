package aitoolproxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/quota"
	"github.com/anne-x/hive/internal/rank"
	"github.com/anne-x/hive/internal/rpc"
)

func setup(t *testing.T) (*Proxy, func()) {
	t.Helper()
	q := quota.New(0)
	ctx, cancel := context.WithCancel(context.Background())
	go q.Run(ctx)
	ws := t.TempDir()
	// Set a generous quota so tests that aren't about quota don't exhaust.
	q.SetLimit(context.Background(), quota.Key{
		RoomID: "r", Agent: "a", Resource: "ai_tool:claude-code",
	}, 100)
	return &Proxy{
		RoomID:    "r",
		AgentName: "a",
		Rank:      &rank.Rank{Name: "staff", AIToolAllowed: true},
		Quota:     q,
		Provider:  MockProvider{},
		Workspace: ws,
	}, cancel
}

func enc(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestInvoke_MockEcho(t *testing.T) {
	p, cancel := setup(t)
	defer cancel()

	out, err := p.Invoke(context.Background(), enc(t, rpc.AIToolInvokeParams{
		Tool:   "claude-code",
		Prompt: "hello world",
	}))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	res := out.(rpc.AIToolInvokeResult)
	if !strings.Contains(res.Output, "hello world") {
		t.Fatalf("mock didn't echo prompt: %q", res.Output)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code: %d", res.ExitCode)
	}
}

func TestInvoke_WritesToWorkspace(t *testing.T) {
	// The MockProvider leaves a breadcrumb under cwd; that breadcrumb
	// doubles as a test fixture proving cwd pinning is wired.
	p, cancel := setup(t)
	defer cancel()

	_, err := p.Invoke(context.Background(), enc(t, rpc.AIToolInvokeParams{Prompt: "x"}))
	if err != nil {
		t.Fatal(err)
	}
	breadcrumb := filepath.Join(p.Workspace, ".hive-ai-tool-mock.log")
	if _, err := os.Stat(breadcrumb); err != nil {
		t.Fatalf("mock should have written %s: %v", breadcrumb, err)
	}
}

func TestInvoke_RankGate(t *testing.T) {
	p, cancel := setup(t)
	defer cancel()
	p.Rank = &rank.Rank{Name: "intern", AIToolAllowed: false}

	_, err := p.Invoke(context.Background(), enc(t, rpc.AIToolInvokeParams{Prompt: "x"}))
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrCodePermissionDenied {
		t.Fatalf("want permission_denied, got %v", err)
	}
}

func TestInvoke_QuotaExhaust(t *testing.T) {
	p, cancel := setup(t)
	defer cancel()
	// Tighten the limit so we exhaust after one call.
	p.Quota.SetLimit(context.Background(), quota.Key{
		RoomID: "r", Agent: "a", Resource: "ai_tool:claude-code",
	}, 1)

	if _, err := p.Invoke(context.Background(), enc(t, rpc.AIToolInvokeParams{Prompt: "first"})); err != nil {
		t.Fatalf("first call must succeed: %v", err)
	}
	_, err := p.Invoke(context.Background(), enc(t, rpc.AIToolInvokeParams{Prompt: "second"}))
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrCodeQuotaExceeded {
		t.Fatalf("want quota_exceeded, got %v", err)
	}
}

func TestInvoke_NoProvider(t *testing.T) {
	p, cancel := setup(t)
	defer cancel()
	p.Provider = nil

	_, err := p.Invoke(context.Background(), enc(t, rpc.AIToolInvokeParams{Prompt: "x"}))
	if err == nil || !strings.Contains(err.Error(), "no ai-tool Provider") {
		t.Fatalf("expected 'no Provider' error, got %v", err)
	}
}

func TestInvoke_WrongToolName(t *testing.T) {
	p, cancel := setup(t)
	defer cancel()
	_, err := p.Invoke(context.Background(), enc(t, rpc.AIToolInvokeParams{
		Tool:   "cursor-cli",
		Prompt: "x",
	}))
	if err == nil || !strings.Contains(err.Error(), "not available") {
		t.Fatalf("expected 'not available' error, got %v", err)
	}
}

func TestInvoke_DefaultToolName(t *testing.T) {
	// Empty Tool field should default to Provider.Name().
	p, cancel := setup(t)
	defer cancel()
	_, err := p.Invoke(context.Background(), enc(t, rpc.AIToolInvokeParams{Prompt: "x"}))
	if err != nil {
		t.Fatalf("empty tool should default to provider name: %v", err)
	}
}

func TestNewClaudeCodeFromEnv_MissingKey(t *testing.T) {
	os.Unsetenv("ANTHROPIC_API_KEY")
	if p := NewClaudeCodeFromEnv(); p != nil {
		t.Fatal("expected nil provider without API key")
	}
}
