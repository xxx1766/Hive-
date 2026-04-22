package llmproxy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/quota"
	"github.com/anne-x/hive/internal/rank"
	"github.com/anne-x/hive/internal/rpc"
)

func setup(t *testing.T) (*Proxy, context.CancelFunc) {
	t.Helper()
	q := quota.New(0)
	ctx, cancel := context.WithCancel(context.Background())
	go q.Run(ctx)
	return &Proxy{
		RoomID:    "room-A",
		AgentName: "summarize",
		Rank: &rank.Rank{
			Name:       "staff",
			LLMAllowed: true,
			Quota:      rank.Quota{Tokens: map[string]int{"gpt-4o-mini": 100}},
		},
		Quota:    q,
		Provider: MockProvider{},
	}, cancel
}

func encode(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMockCompleteReturnsText(t *testing.T) {
	p, cancel := setup(t)
	defer cancel()
	// Install the limit in the actor (demo daemon does this via
	// installAgentProxies; test replicates).
	p.Quota.SetLimit(context.Background(), quota.Key{
		RoomID: p.RoomID, Agent: p.AgentName, Resource: "tokens:gpt-4o-mini",
	}, 100)

	raw := encode(t, rpc.LLMCompleteParams{
		Model: "gpt-4o-mini",
		Messages: []rpc.LLMMessage{
			{Role: "user", Content: "hello"},
		},
	})
	out, err := p.Complete(context.Background(), raw)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	res := out.(rpc.LLMCompleteResult)
	if !strings.Contains(res.Text, "hello") {
		t.Fatalf("mock didn't echo user content: %q", res.Text)
	}
	if res.Usage.TotalTokens == 0 {
		t.Fatal("usage.total_tokens should be non-zero")
	}
}

func TestQuotaExhaustsAfterManyCalls(t *testing.T) {
	p, cancel := setup(t)
	defer cancel()
	p.Quota.SetLimit(context.Background(), quota.Key{
		RoomID: p.RoomID, Agent: p.AgentName, Resource: "tokens:gpt-4o-mini",
	}, 50) // tight budget — a few mock calls will exhaust

	raw := encode(t, rpc.LLMCompleteParams{
		Model:    "gpt-4o-mini",
		Messages: []rpc.LLMMessage{{Role: "user", Content: "long enough to matter"}},
	})
	var lastErr error
	for i := 0; i < 20; i++ {
		_, err := p.Complete(context.Background(), raw)
		if err != nil {
			lastErr = err
			break
		}
	}
	if lastErr == nil {
		t.Fatal("expected quota exhaustion within 20 calls")
	}
	var perr *protocol.Error
	if !errors.As(lastErr, &perr) || perr.Code != protocol.ErrCodeQuotaExceeded {
		t.Fatalf("want quota_exceeded error, got %v", lastErr)
	}
}

func TestLLMDeniedByRank(t *testing.T) {
	p, cancel := setup(t)
	defer cancel()
	p.Rank = &rank.Rank{Name: "intern", LLMAllowed: false}

	raw := encode(t, rpc.LLMCompleteParams{
		Model:    "gpt-4o-mini",
		Messages: []rpc.LLMMessage{{Role: "user", Content: "anything"}},
	})
	_, err := p.Complete(context.Background(), raw)
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrCodePermissionDenied {
		t.Fatalf("want permission_denied, got %v", err)
	}
}

func TestCrossRoomTokenIsolation(t *testing.T) {
	// Same agent name, different rooms: their token buckets are independent.
	q := quota.New(0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)

	pA := &Proxy{
		RoomID: "A", AgentName: "sum", Rank: &rank.Rank{LLMAllowed: true},
		Quota: q, Provider: MockProvider{},
	}
	pB := *pA
	pB.RoomID = "B"

	kA := quota.Key{RoomID: "A", Agent: "sum", Resource: "tokens:gpt-4o-mini"}
	kB := quota.Key{RoomID: "B", Agent: "sum", Resource: "tokens:gpt-4o-mini"}
	q.SetLimit(ctx, kA, 30)
	q.SetLimit(ctx, kB, 30)

	raw := encode(t, rpc.LLMCompleteParams{
		Model:    "gpt-4o-mini",
		Messages: []rpc.LLMMessage{{Role: "user", Content: "content that will cost some tokens"}},
	})

	// Drain A.
	for i := 0; i < 20; i++ {
		if _, err := pA.Complete(ctx, raw); err != nil {
			break
		}
	}

	// B should still have budget.
	if _, err := pB.Complete(ctx, raw); err != nil {
		t.Fatalf("B should still be fundable, got %v", err)
	}
}
