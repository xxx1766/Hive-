// Package runners holds code shared between the built-in Agent runners
// that ship with Hive (hive-skill-runner, hive-workflow-runner, future
// ones). Keeping tool dispatch in one place means a new tool only needs
// to be wired here — both runners pick it up automatically.
package runners

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	hive "github.com/anne-x/hive/sdk/go"
)

// Tool groups used by manifests' `tools: [...]` allow-list. A tool name
// only dispatches if its group is in the list.
const (
	GroupNet    = "net"
	GroupFS     = "fs"
	GroupPeer   = "peer"
	GroupLLM    = "llm"
	GroupMemory = "memory"
	GroupAITool = "ai_tool"
)

// ToolGroup classifies a tool name into its group, or "" if unknown.
func ToolGroup(tool string) string {
	switch {
	case tool == "net_fetch":
		return GroupNet
	case strings.HasPrefix(tool, "fs_"):
		return GroupFS
	case tool == "peer_send":
		return GroupPeer
	case tool == "llm_complete":
		return GroupLLM
	case strings.HasPrefix(tool, "memory_"):
		return GroupMemory
	case tool == "ai_tool_invoke":
		return GroupAITool
	}
	return ""
}

// ToolAllowed reports whether a tool's group is in the allow-list.
func ToolAllowed(tool string, allowed []string) bool {
	g := ToolGroup(tool)
	if g == "" {
		return false
	}
	for _, x := range allowed {
		if x == g {
			return true
		}
	}
	return false
}

// DispatchTool invokes a tool by name and returns its result as
// structured data (primitive / map / slice). Callers that need a
// textual representation (e.g. to feed an LLM) call ResultText.
//
// Errors bubble from Hive's proxies — permission denied, quota
// exhausted, etc. — so the runner logs them in its own voice.
func DispatchTool(ctx context.Context, a *hive.Agent, name string, args map[string]any) (any, error) {
	switch name {
	case "net_fetch":
		url := getString(args, "url")
		if url == "" {
			return nil, fmt.Errorf("net_fetch: url is required")
		}
		method := getString(args, "method")
		if method == "" {
			method = "GET"
		}
		var body []byte
		if s := getString(args, "body"); s != "" {
			body = []byte(s)
		}
		status, respBody, err := a.NetFetch(ctx, method, url, getStringMap(args, "headers"), body)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"status": status,
			"body":   string(respBody),
		}, nil

	case "fs_read":
		path := getString(args, "path")
		if path == "" {
			return nil, fmt.Errorf("fs_read: path is required")
		}
		data, err := a.FSRead(ctx, path)
		if err != nil {
			return nil, err
		}
		return string(data), nil

	case "fs_write":
		path := getString(args, "path")
		content := getString(args, "content")
		if path == "" {
			return nil, fmt.Errorf("fs_write: path is required")
		}
		if err := a.FSWrite(ctx, path, []byte(content)); err != nil {
			return nil, err
		}
		return "ok", nil

	case "fs_list":
		path := getString(args, "path")
		if path == "" {
			return nil, fmt.Errorf("fs_list: path is required")
		}
		entries, err := a.FSList(ctx, path)
		if err != nil {
			return nil, err
		}
		out := make([]any, len(entries))
		for i, e := range entries {
			out[i] = map[string]any{"name": e.Name, "is_dir": e.IsDir, "size": e.Size}
		}
		return out, nil

	case "peer_send":
		to := getString(args, "to")
		if to == "" {
			return nil, fmt.Errorf("peer_send: to is required")
		}
		if err := a.PeerSend(ctx, to, args["payload"]); err != nil {
			return nil, err
		}
		return "sent", nil

	case "memory_put":
		scope := getString(args, "scope")
		key := getString(args, "key")
		// value may be a string (preferred) or bytes — coerce to string.
		value := getString(args, "value")
		if err := a.MemoryPut(ctx, scope, key, []byte(value)); err != nil {
			return nil, err
		}
		return "ok", nil

	case "memory_get":
		scope := getString(args, "scope")
		key := getString(args, "key")
		val, exists, err := a.MemoryGet(ctx, scope, key)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"exists": exists,
			"value":  string(val),
		}, nil

	case "memory_list":
		scope := getString(args, "scope")
		prefix := getString(args, "prefix")
		keys, err := a.MemoryList(ctx, scope, prefix)
		if err != nil {
			return nil, err
		}
		return map[string]any{"keys": keys}, nil

	case "memory_delete":
		scope := getString(args, "scope")
		key := getString(args, "key")
		if err := a.MemoryDelete(ctx, scope, key); err != nil {
			return nil, err
		}
		return "ok", nil

	case "ai_tool_invoke":
		tool := getString(args, "tool")
		if tool == "" {
			tool = "claude-code"
		}
		prompt := getString(args, "prompt")
		if prompt == "" {
			return nil, fmt.Errorf("ai_tool_invoke: prompt is required")
		}
		out, err := a.AIToolInvoke(ctx, tool, prompt)
		if err != nil {
			return nil, err
		}
		return map[string]any{"output": out}, nil

	case "llm_complete":
		model := getString(args, "model")
		if model == "" {
			model = "gpt-4o-mini"
		}
		maxTok := getInt(args, "max_tokens")
		text, usage, err := a.LLMComplete(ctx, "", model, messagesFromArgs(args), maxTok)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"text": text,
			"usage": map[string]any{
				"prompt_tokens":     usage.PromptTokens,
				"completion_tokens": usage.CompletionTokens,
				"total_tokens":      usage.TotalTokens,
			},
		}, nil

	default:
		return nil, fmt.Errorf("unknown tool: %q", name)
	}
}

// ResultText flattens any structured tool result into a compact JSON
// string (for inclusion in an LLM context window). `max` caps the
// string length; 0 disables truncation.
func ResultText(v any, max int) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	s := string(b)
	if max > 0 && len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// ── tiny arg coercion helpers ─────────────────────────────────────────────

func getString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func getStringMap(m map[string]any, key string) map[string]string {
	raw, _ := m[key].(map[string]any)
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// getInt coerces JSON numbers (unmarshalled as float64 by default) and
// plain ints to int. Anything else returns 0.
func getInt(m map[string]any, key string) int {
	switch t := m[key].(type) {
	case int:
		return t
	case int64:
		return int(t)
	case float64:
		return int(t)
	}
	return 0
}

func messagesFromArgs(m map[string]any) []hive.LLMMessage {
	raw, _ := m["messages"].([]any)
	out := make([]hive.LLMMessage, 0, len(raw))
	for _, item := range raw {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, hive.LLMMessage{
			Role:    getString(obj, "role"),
			Content: getString(obj, "content"),
		})
	}
	return out
}
