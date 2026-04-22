// hive-skill-runner is Hive's built-in executor for `kind: skill` Agents.
//
// Invocation contract (set by internal/daemon.prepareSkillImage):
//
//	Binary:      /app/__hive_runner__   (hardlinked into the Image dir)
//	Env:
//	  HIVE_SKILL_PATH   — absolute path inside sandbox to SKILL.md
//	  HIVE_SKILL_MODEL  — preferred LLM model (empty ⇒ daemon default)
//	  HIVE_SKILL_TOOLS  — comma-separated allow-list of tool groups
//	                     (net, fs, peer, llm)
//	Stdin/stdout: JSON-RPC 2.0 to hived, via sdk/go.
//
// For each task/run the runner:
//  1. loads SKILL.md once
//  2. builds a two-message conversation (system = SKILL.md + tool schema,
//     user = task input)
//  3. iterates up to maxSkillIterations times:
//     - calls llm/complete
//     - parses the model's reply as {"tool": ..., "args": ...}
//     or {"answer": ...}
//     - if tool: dispatches through Hive's proxy (net_fetch → a.NetFetch,
//     fs_read → a.FSRead, etc.); appends the tool's result as a new
//     user message; loops
//     - if answer: replies task/done with it; exits the loop
//  4. if the loop hits the iteration cap without an answer, fails the task.
//
// The runner itself is a normal Hive Agent — same sandbox, same Rank, same
// quota accounting. The isolation story is unchanged.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	hive "github.com/anne-x/hive/sdk/go"
)

const maxSkillIterations = 20

func main() {
	a := hive.MustConnect()
	defer a.Close()

	skillPath := os.Getenv("HIVE_SKILL_PATH")
	if skillPath == "" {
		a.Log("error", "HIVE_SKILL_PATH is unset; nothing to run")
		os.Exit(2)
	}
	skillBytes, err := os.ReadFile(skillPath)
	if err != nil {
		a.Log("error", "cannot read skill", map[string]any{"path": skillPath, "err": err.Error()})
		os.Exit(2)
	}
	skill := string(skillBytes)

	model := os.Getenv("HIVE_SKILL_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	tools := parseCSV(os.Getenv("HIVE_SKILL_TOOLS"))
	if len(tools) == 0 {
		tools = []string{"net", "fs", "peer"}
	}

	a.Log("info", "skill runner ready", map[string]any{
		"skill_bytes": len(skillBytes),
		"model":       model,
		"tools":       tools,
	})

	ctx := context.Background()
	system := buildSystemPrompt(skill, tools)

	for task := range a.Tasks() {
		runOne(ctx, a, task, system, model, tools)
	}
}

func runOne(ctx context.Context, a *hive.Agent, task *hive.Task, system, model string, tools []string) {
	a.Log("info", "skill task received", map[string]any{"task_id": task.ID})
	msgs := []hive.LLMMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: string(task.Input)},
	}

	for iter := 1; iter <= maxSkillIterations; iter++ {
		text, _, err := a.LLMComplete(ctx, "", model, msgs, 512)
		if err != nil {
			a.Log("error", "llm call failed", map[string]any{"iter": iter, "err": err.Error()})
			_ = task.Fail(2, err.Error())
			return
		}
		msgs = append(msgs, hive.LLMMessage{Role: "assistant", Content: text})

		parsed := parseReply(text)

		if parsed.Answer != "" {
			_ = task.Reply(map[string]any{"answer": parsed.Answer, "iterations": iter})
			return
		}
		if parsed.Tool == "" {
			// Fallback: treat a non-JSON response as the final answer.
			// This keeps the mock provider path usable — mock returns plain
			// text, which we surface verbatim instead of failing.
			_ = task.Reply(map[string]any{"answer": strings.TrimSpace(text), "iterations": iter, "format": "plain"})
			return
		}

		if !toolAllowed(parsed.Tool, tools) {
			a.Log("warn", "tool not allowed by manifest", map[string]any{"tool": parsed.Tool, "allowed": tools})
			msgs = append(msgs, hive.LLMMessage{
				Role:    "user",
				Content: fmt.Sprintf("tool %q is not in the allow-list %v", parsed.Tool, tools),
			})
			continue
		}

		result, terr := dispatchTool(ctx, a, parsed.Tool, parsed.Args)
		if terr != nil {
			a.Log("error", "tool failed", map[string]any{"tool": parsed.Tool, "err": terr.Error()})
			msgs = append(msgs, hive.LLMMessage{
				Role:    "user",
				Content: fmt.Sprintf("tool %s failed: %s", parsed.Tool, terr.Error()),
			})
			continue
		}
		a.Log("info", "tool ok", map[string]any{"tool": parsed.Tool, "result_bytes": len(result)})
		msgs = append(msgs, hive.LLMMessage{
			Role:    "user",
			Content: fmt.Sprintf("tool %s returned: %s", parsed.Tool, result),
		})
	}

	_ = task.Fail(3, fmt.Sprintf("skill did not converge within %d iterations", maxSkillIterations))
}

// ── LLM response parsing ─────────────────────────────────────────────────

type reply struct {
	Tool   string         `json:"tool,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
	Answer string         `json:"answer,omitempty"`
}

// jsonObjRe matches the first top-level JSON object in a string. LLMs often
// wrap JSON in prose or ```json fences, so we pluck the object out.
var jsonObjRe = regexp.MustCompile(`\{(?s).*\}`)

func parseReply(text string) reply {
	trimmed := strings.TrimSpace(text)
	// Try direct parse first (common for well-behaved models).
	var r reply
	if err := json.Unmarshal([]byte(trimmed), &r); err == nil {
		if r.Tool != "" || r.Answer != "" {
			return r
		}
	}
	// Fall back: pull the outermost { ... } block.
	if m := jsonObjRe.FindString(trimmed); m != "" {
		var r2 reply
		if err := json.Unmarshal([]byte(m), &r2); err == nil {
			if r2.Tool != "" || r2.Answer != "" {
				return r2
			}
		}
	}
	return reply{}
}

// ── Tool dispatch ─────────────────────────────────────────────────────────

// toolAllowed checks whether the tool belongs to one of the allow-list
// groups. Groups: net (net_fetch), fs (fs_read/fs_write/fs_list),
// peer (peer_send), llm (llm_complete — though skills rarely need it
// since they already drive an LLM loop).
func toolAllowed(tool string, allowed []string) bool {
	group := toolGroup(tool)
	if group == "" {
		return false
	}
	for _, g := range allowed {
		if g == group {
			return true
		}
	}
	return false
}

func toolGroup(tool string) string {
	switch {
	case tool == "net_fetch":
		return "net"
	case strings.HasPrefix(tool, "fs_"):
		return "fs"
	case tool == "peer_send":
		return "peer"
	case tool == "llm_complete":
		return "llm"
	}
	return ""
}

func dispatchTool(ctx context.Context, a *hive.Agent, name string, args map[string]any) (string, error) {
	switch name {
	case "net_fetch":
		url, _ := args["url"].(string)
		if url == "" {
			return "", fmt.Errorf("net_fetch: url is required")
		}
		status, body, err := a.NetFetch(ctx, "GET", url, nil, nil)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(`{"status":%d,"body":%q}`, status, truncate(string(body), 1000)), nil

	case "fs_read":
		path, _ := args["path"].(string)
		if path == "" {
			return "", fmt.Errorf("fs_read: path is required")
		}
		data, err := a.FSRead(ctx, path)
		if err != nil {
			return "", err
		}
		return truncate(string(data), 2000), nil

	case "fs_write":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		if path == "" {
			return "", fmt.Errorf("fs_write: path is required")
		}
		if err := a.FSWrite(ctx, path, []byte(content)); err != nil {
			return "", err
		}
		return "ok", nil

	case "fs_list":
		path, _ := args["path"].(string)
		if path == "" {
			return "", fmt.Errorf("fs_list: path is required")
		}
		entries, err := a.FSList(ctx, path)
		if err != nil {
			return "", err
		}
		b, _ := json.Marshal(entries)
		return string(b), nil

	case "peer_send":
		to, _ := args["to"].(string)
		if to == "" {
			return "", fmt.Errorf("peer_send: to is required")
		}
		payload := args["payload"]
		if err := a.PeerSend(ctx, to, payload); err != nil {
			return "", err
		}
		return "sent", nil

	default:
		return "", fmt.Errorf("unknown tool: %q", name)
	}
}

// ── Prompt construction ──────────────────────────────────────────────────

func buildSystemPrompt(skill string, tools []string) string {
	var b strings.Builder
	b.WriteString(skill)
	b.WriteString("\n\n--- Hive runtime instructions ---\n\n")
	b.WriteString("You are running inside Hive. You MUST respond with a single JSON object and nothing else.\n")
	b.WriteString("Two reply shapes:\n")
	b.WriteString(`  {"tool": "<name>", "args": {...}}   — call a tool; you'll get the result back in the next user turn` + "\n")
	b.WriteString(`  {"answer": "<text>"}                — final answer; Hive stops the loop` + "\n\n")
	if containsAny(tools, []string{"net"}) {
		b.WriteString(`Tool: net_fetch — args {"url": string}; returns JSON {"status":int,"body":string}` + "\n")
	}
	if containsAny(tools, []string{"fs"}) {
		b.WriteString(`Tool: fs_read  — args {"path": string}; returns file contents` + "\n")
		b.WriteString(`Tool: fs_write — args {"path": string, "content": string}; returns "ok"` + "\n")
		b.WriteString(`Tool: fs_list  — args {"path": string}; returns JSON array of entries` + "\n")
	}
	if containsAny(tools, []string{"peer"}) {
		b.WriteString(`Tool: peer_send — args {"to": string, "payload": any}; sends a message to another Agent in the same Room` + "\n")
	}
	return b.String()
}

// ── tiny helpers ──────────────────────────────────────────────────────────

func parseCSV(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func containsAny(hay, needles []string) bool {
	set := map[string]bool{}
	for _, h := range hay {
		set[h] = true
	}
	for _, n := range needles {
		if set[n] {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
