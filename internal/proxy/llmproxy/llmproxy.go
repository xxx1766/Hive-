// Package llmproxy routes Agent llm/complete requests through a Provider.
//
// Separating the Provider interface from the Proxy itself lets us swap in:
//   - mock: deterministic, no network, for CI and offline demo
//   - openai: OpenAI-compatible REST (any vendor that speaks that dialect)
//
// Token accounting uses the Usage block that providers return (OpenAI-native
// or approximated). We do NOT tokenize on our own side — too much risk of
// drifting from the provider's accounting.
package llmproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/anne-x/hive/internal/protocol"
	"github.com/anne-x/hive/internal/proxy/netproxy"
	"github.com/anne-x/hive/internal/quota"
	"github.com/anne-x/hive/internal/rank"
	"github.com/anne-x/hive/internal/rpc"
)

// Provider is the pluggable LLM backend. Demo ships `mock` and `openai`.
type Provider interface {
	Name() string
	Complete(ctx context.Context, p rpc.LLMCompleteParams) (rpc.LLMCompleteResult, error)
}

// Proxy is built per-Agent.
type Proxy struct {
	RoomID    string
	AgentName string
	Rank      *rank.Rank
	Quota     *quota.Actor
	Provider  Provider // may be nil ⇒ reject llm/complete calls
}

// Complete handles an Agent's llm/complete call.
func (p *Proxy) Complete(ctx context.Context, params json.RawMessage) (any, error) {
	var cp rpc.LLMCompleteParams
	if err := json.Unmarshal(params, &cp); err != nil {
		return nil, protocol.NewError(protocol.ErrCodeInvalidParams, err.Error())
	}
	if !p.Rank.LLMAllowed {
		return nil, protocol.ErrPermissionDenied("rank " + p.Rank.Name + " cannot call llm/complete")
	}
	if p.Provider == nil {
		return nil, protocol.NewError(protocol.ErrCodeInternal, "no LLM provider configured")
	}

	if cp.Provider == "" {
		cp.Provider = p.Provider.Name()
	}
	if cp.Model == "" {
		cp.Model = "gpt-4o-mini"
	}

	res, err := p.Provider.Complete(ctx, cp)
	if err != nil {
		return nil, fmt.Errorf("llm/complete: %w", err)
	}

	// Charge total tokens against (Room, Agent, model).
	// Provider isn't in the key — the Rank caps "tokens of model X",
	// regardless of which backend served them.
	resKey := quota.Key{
		RoomID:   p.RoomID,
		Agent:    p.AgentName,
		Resource: "tokens:" + cp.Model,
	}
	qr, qerr := p.Quota.Consume(ctx, resKey, res.Usage.TotalTokens)
	if qerr != nil {
		return nil, qerr
	}
	if !qr.Allowed {
		return nil, protocol.ErrQuotaExceeded(fmt.Sprintf(
			"token quota for %s exhausted after this call (tried to consume %d, remaining %d)",
			resKey.Resource, res.Usage.TotalTokens, qr.Remaining))
	}

	return res, nil
}

// ── Mock provider ────────────────────────────────────────────────────────

type MockProvider struct{}

func (MockProvider) Name() string { return "mock" }

func (MockProvider) Complete(ctx context.Context, p rpc.LLMCompleteParams) (rpc.LLMCompleteResult, error) {
	// Echo the last user message with a prefix; cheap, deterministic, and
	// exercises the full wire path.
	var last string
	for _, m := range p.Messages {
		if m.Role == "user" {
			last = m.Content
		}
	}
	text := "mock-summary: " + truncate(last, 200)
	return rpc.LLMCompleteResult{
		Text: text,
		// Approximate: 4 chars per token (close enough for demo metering).
		Usage: rpc.LLMUsage{
			PromptTokens:     approxTokens(p.Messages),
			CompletionTokens: len(text) / 4,
			TotalTokens:      approxTokens(p.Messages) + len(text)/4,
		},
	}, nil
}

// ── OpenAI-compatible provider ────────────────────────────────────────────

// OpenAIProvider targets /v1/chat/completions. Works against OpenAI itself
// and any vendor that imitates the schema (Together, Groq, DeepSeek, etc.).
type OpenAIProvider struct {
	APIKey  string
	BaseURL string // default "https://api.openai.com/v1"
}

// NewOpenAIFromEnv builds a provider from OPENAI_API_KEY + OPENAI_BASE_URL.
// Returns nil (not an error) if OPENAI_API_KEY is unset — the daemon will
// fall back to Mock so the demo can run without credentials.
func NewOpenAIFromEnv() *OpenAIProvider {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil
	}
	base := os.Getenv("OPENAI_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	return &OpenAIProvider{APIKey: key, BaseURL: strings.TrimRight(base, "/")}
}

func (o *OpenAIProvider) Name() string { return "openai" }

func (o *OpenAIProvider) Complete(ctx context.Context, p rpc.LLMCompleteParams) (rpc.LLMCompleteResult, error) {
	type oaMsg struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	reqBody := struct {
		Model    string  `json:"model"`
		Messages []oaMsg `json:"messages"`
		MaxTok   int     `json:"max_tokens,omitempty"`
	}{Model: p.Model, MaxTok: p.MaxTokens}
	for _, m := range p.Messages {
		reqBody.Messages = append(reqBody.Messages, oaMsg{Role: m.Role, Content: m.Content})
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return rpc.LLMCompleteResult{}, err
	}
	url := o.BaseURL + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return rpc.LLMCompleteResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+o.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := netproxy.SharedClient().Do(req)
	if err != nil {
		return rpc.LLMCompleteResult{}, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return rpc.LLMCompleteResult{}, err
	}
	if resp.StatusCode >= 400 {
		return rpc.LLMCompleteResult{}, fmt.Errorf("openai %d: %s", resp.StatusCode, string(body))
	}

	var ck struct {
		Choices []struct {
			Message struct{ Content string } `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &ck); err != nil {
		return rpc.LLMCompleteResult{}, fmt.Errorf("parse openai response: %w", err)
	}
	text := ""
	if len(ck.Choices) > 0 {
		text = ck.Choices[0].Message.Content
	}
	return rpc.LLMCompleteResult{
		Text: text,
		Usage: rpc.LLMUsage{
			PromptTokens:     ck.Usage.PromptTokens,
			CompletionTokens: ck.Usage.CompletionTokens,
			TotalTokens:      ck.Usage.TotalTokens,
		},
	}, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func approxTokens(ms []rpc.LLMMessage) int {
	total := 0
	for _, m := range ms {
		total += len(m.Content) / 4
	}
	return total
}
