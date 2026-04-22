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
// Algorithm: ReAct-lite JSON loop. Each iteration prompts the LLM with
// the full conversation so far; the model must reply as either
//
//	{"tool": "<name>", "args": {...}}    — dispatched via internal/runners
//	{"answer": "<text>"}                 — terminates; replies task/done
//
// Plain-text (non-JSON) replies are treated as the final answer so the
// mock LLM provider path still produces a legible demo output. The
// runner itself is a normal Hive Agent — same sandbox, same Rank, same
// quota accounting; isolation is not weakened.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/anne-x/hive/internal/runners"
	hive "github.com/anne-x/hive/sdk/go"
)

const (
	maxSkillIterations = 20
	toolResultCap      = 2000 // bytes of tool result shown to the LLM
)

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

	model := os.Getenv("HIVE_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	tools := parseCSV(os.Getenv("HIVE_TOOLS"))
	if len(tools) == 0 {
		tools = []string{runners.GroupNet, runners.GroupFS, runners.GroupPeer}
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
			// Fallback: non-JSON reply → surface verbatim as the answer.
			// Keeps the mock provider path usable end-to-end.
			_ = task.Reply(map[string]any{
				"answer":     strings.TrimSpace(text),
				"iterations": iter,
				"format":     "plain",
			})
			return
		}

		if !runners.ToolAllowed(parsed.Tool, tools) {
			a.Log("warn", "tool not allowed by manifest", map[string]any{
				"tool": parsed.Tool, "allowed": tools,
			})
			msgs = append(msgs, hive.LLMMessage{
				Role:    "user",
				Content: fmt.Sprintf("tool %q is not in the allow-list %v", parsed.Tool, tools),
			})
			continue
		}

		result, terr := runners.DispatchTool(ctx, a, parsed.Tool, parsed.Args)
		if terr != nil {
			a.Log("error", "tool failed", map[string]any{"tool": parsed.Tool, "err": terr.Error()})
			msgs = append(msgs, hive.LLMMessage{
				Role:    "user",
				Content: fmt.Sprintf("tool %s failed: %s", parsed.Tool, terr.Error()),
			})
			continue
		}
		a.Log("info", "tool ok", map[string]any{"tool": parsed.Tool})
		msgs = append(msgs, hive.LLMMessage{
			Role:    "user",
			Content: fmt.Sprintf("tool %s returned: %s", parsed.Tool, runners.ResultText(result, toolResultCap)),
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
	var r reply
	if err := json.Unmarshal([]byte(trimmed), &r); err == nil {
		if r.Tool != "" || r.Answer != "" {
			return r
		}
	}
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

// ── Prompt construction ──────────────────────────────────────────────────

func buildSystemPrompt(skill string, tools []string) string {
	var b strings.Builder
	b.WriteString(skill)
	b.WriteString("\n\n--- Hive runtime instructions ---\n\n")
	b.WriteString("You are running inside Hive. You MUST respond with a single JSON object and nothing else.\n")
	b.WriteString("Two reply shapes:\n")
	b.WriteString(`  {"tool": "<name>", "args": {...}}   — call a tool; you'll get the result back in the next user turn` + "\n")
	b.WriteString(`  {"answer": "<text>"}                — final answer; Hive stops the loop` + "\n\n")
	if containsAny(tools, []string{runners.GroupNet}) {
		b.WriteString(`Tool: net_fetch — args {"url": string}; returns JSON {"status":int,"body":string}` + "\n")
	}
	if containsAny(tools, []string{runners.GroupFS}) {
		b.WriteString(`Tool: fs_read  — args {"path": string}; returns file contents` + "\n")
		b.WriteString(`Tool: fs_write — args {"path": string, "content": string}; returns "ok"` + "\n")
		b.WriteString(`Tool: fs_list  — args {"path": string}; returns JSON array of entries` + "\n")
	}
	if containsAny(tools, []string{runners.GroupPeer}) {
		b.WriteString(`Tool: peer_send — args {"to": string, "payload": any}; sends a message to another Agent in the same Room` + "\n")
	}
	if containsAny(tools, []string{runners.GroupMemory}) {
		b.WriteString(`Tool: memory_put  — args {"scope": string, "key": string, "value": string}; scope="" is Room-private, scope="<volume>" is cross-Room` + "\n")
		b.WriteString(`Tool: memory_get  — args {"scope": string, "key": string}; returns {"exists":bool,"value":string}` + "\n")
		b.WriteString(`Tool: memory_list — args {"scope": string, "prefix": string}; returns {"keys":[string]}` + "\n")
		b.WriteString(`Tool: memory_delete — args {"scope": string, "key": string}` + "\n")
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
