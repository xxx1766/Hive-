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
//
// Two ingestion paths (since v0.2):
//
//	Tasks()  — the classic `hive run --target <agent>` dispatch path
//	Peers()  — Conversation hops from another Agent in the same Room.
//	           When a peer message carries a non-empty ConvID, the runner
//	           treats the payload like a task input, runs the same ReAct
//	           loop, and replies by PeerSend(from, ..., WithConv(convID))
//	           so the round counter advances on the right transcript.
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
	// llmMaxCompletionTokens caps each ReAct turn's response. Sized for
	// long-form writing tasks (e.g. paper-writer drafting an 800-word
	// section in one shot). Tool-call turns only use ~30 tokens; reasoning
	// models like gpt-5 may consume non-trivial reasoning_tokens out of
	// this budget — keep the cap generous.
	llmMaxCompletionTokens = 4000
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

	for {
		select {
		case task, ok := <-a.Tasks():
			if !ok {
				return
			}
			runOne(ctx, a, task, system, model, tools)
		case peer, ok := <-a.Peers():
			if !ok {
				return
			}
			runFromPeer(ctx, a, peer, system, model, tools)
		case <-a.Done():
			return
		}
	}
}

// runOne handles a top-level task/run dispatch. Final answer goes back
// via task/done; failures via task/error.
func runOne(ctx context.Context, a *hive.Agent, task *hive.Task, system, model string, tools []string) {
	a.Log("info", "skill task received", map[string]any{"task_id": task.ID, "conv_id": task.ConvID})
	out, err := react(ctx, a, system, model, tools, string(task.Input), task.ConvID)
	if err != nil {
		_ = task.Fail(2, err.Error())
		return
	}
	_ = task.Reply(out)
}

// runFromPeer treats an inbound peer/recv with a non-empty ConvID as a
// follow-up task in a Conversation. Same ReAct loop, but the final
// answer is shipped back to the sender as a peer/send hop (still
// counted against the round budget). Peer messages without ConvID
// (ad-hoc inter-Agent chat) are surfaced as a log and dropped, since
// they have no transcript and no reply contract.
func runFromPeer(ctx context.Context, a *hive.Agent, peer *hive.PeerMessage, system, model string, tools []string) {
	if peer.ConvID == "" {
		a.Log("info", "peer message dropped (no conv_id)", map[string]any{"from": peer.From})
		return
	}
	a.Log("info", "skill peer received", map[string]any{"from": peer.From, "conv_id": peer.ConvID})

	out, err := react(ctx, a, system, model, tools, string(peer.Payload), peer.ConvID)
	if err != nil {
		// Surface the failure as a peer reply so the originating Agent
		// can choose to retry or give up. Wrapping in a struct keeps the
		// schema introspectable.
		_ = a.PeerSend(ctx, peer.From, map[string]any{"error": err.Error()}, hive.WithConv(peer.ConvID))
		return
	}
	if err := a.PeerSend(ctx, peer.From, out, hive.WithConv(peer.ConvID)); err != nil {
		a.Log("error", "peer reply failed", map[string]any{"err": err.Error(), "conv_id": peer.ConvID})
	}
}

// react drives the ReAct loop. Returns the parsed answer (a map) on
// success, or an error if the loop never converged. convID is threaded
// through so any peer_send tool calls the LLM emits also count against
// the right Conversation transcript.
func react(ctx context.Context, a *hive.Agent, system, model string, tools []string, userInput, convID string) (map[string]any, error) {
	msgs := []hive.LLMMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: userInput},
	}

	for iter := 1; iter <= maxSkillIterations; iter++ {
		text, _, err := a.LLMComplete(ctx, "", model, msgs, llmMaxCompletionTokens)
		if err != nil {
			a.Log("error", "llm call failed", map[string]any{"iter": iter, "err": err.Error()})
			return nil, err
		}
		msgs = append(msgs, hive.LLMMessage{Role: "assistant", Content: text})

		// Log a snippet of the raw response so post-mortem debugging of
		// "skill returned empty / unexpected" is possible without rerunning.
		// Truncated at 200 chars to keep notification volume bounded.
		a.Log("info", "llm reply", map[string]any{"iter": iter, "len": len(text), "head": truncatePrefix(text, 200)})

		parsed := parseReply(text)

		if parsed.Answer != "" {
			return map[string]any{"answer": parsed.Answer, "iterations": iter}, nil
		}
		if parsed.Tool == "" {
			// Fallback: non-JSON reply → surface verbatim as the answer.
			// Keeps the mock provider path usable end-to-end.
			return map[string]any{
				"answer":     strings.TrimSpace(text),
				"iterations": iter,
				"format":     "plain",
			}, nil
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

		// peer_send is special: when we're in a Conversation we want the
		// hop attributed to its transcript so the round counter advances
		// on the right conv. Inject conv_id into the args before dispatch.
		if parsed.Tool == "peer_send" && convID != "" {
			if parsed.Args == nil {
				parsed.Args = map[string]any{}
			}
			if _, set := parsed.Args["conv_id"]; !set {
				parsed.Args["conv_id"] = convID
			}
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
	return nil, fmt.Errorf("skill did not converge within %d iterations", maxSkillIterations)
}

// ── LLM response parsing ─────────────────────────────────────────────────

type reply struct {
	Tool   string         `json:"tool,omitempty"`
	Args   map[string]any `json:"args,omitempty"`
	Answer string         `json:"answer,omitempty"`
}

// parseReply finds the first parseable {tool}/{answer} JSON object in the
// LLM's output. Three layers, each more permissive than the last:
//
//  1. Whole text is exactly one JSON object (the well-behaved case).
//  2. Whole text contains exactly one outer object — the regex below
//     anchors on the first '{' and the LAST '}' (greedy), good when the
//     LLM wraps the object in prose or ```json fences.
//  3. The text has multiple top-level objects (some models like to "plan"
//     the next 3 tool calls in one reply). We walk the string brace-by-
//     brace, parse each balanced top-level object in turn, and return
//     the first one with a usable shape. Subsequent objects are
//     discarded — the runner does one tool call per turn, so chaining
//     in a single reply is wishful thinking on the model's part.
//
// "Usable shape" means tool != "" || answer != "". Objects that parse
// but carry neither field are skipped (they're probably {"thought":"…"}
// or similar speculative additions).
var jsonObjRe = regexp.MustCompile(`\{(?s).*\}`)

func parseReply(text string) reply {
	trimmed := strings.TrimSpace(text)

	// 1. Exact single-object case.
	var r reply
	if err := json.Unmarshal([]byte(trimmed), &r); err == nil {
		if r.Tool != "" || r.Answer != "" {
			return r
		}
	}

	// 2. Greedy regex (one outer object, possibly wrapped).
	if m := jsonObjRe.FindString(trimmed); m != "" {
		var r2 reply
		if err := json.Unmarshal([]byte(m), &r2); err == nil {
			if r2.Tool != "" || r2.Answer != "" {
				return r2
			}
		}
	}

	// 3. Multi-object fallback: walk the string for balanced top-level
	// objects and return the first one we can parse into a usable shape.
	for _, obj := range topLevelObjects(trimmed) {
		var r3 reply
		if err := json.Unmarshal([]byte(obj), &r3); err == nil {
			if r3.Tool != "" || r3.Answer != "" {
				return r3
			}
		}
	}
	return reply{}
}

// topLevelObjects scans s and yields each balanced top-level JSON object
// substring (in order). Brace counting is naive but JSON-safe enough:
// strings are tracked so braces inside `"..."` don't fool the depth
// counter, and `\"` escapes are handled. Anything outside braces (prose,
// fences, blank lines) is skipped.
func topLevelObjects(s string) []string {
	var out []string
	depth := 0
	start := -1
	inStr := false
	escape := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escape {
			escape = false
			continue
		}
		if inStr {
			switch c {
			case '\\':
				escape = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					out = append(out, s[start:i+1])
					start = -1
				}
			}
		}
	}
	return out
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
		b.WriteString(`Tool: peer_send — args {"to": string, "payload": any}; sends a message to another Agent in the same Room. ` +
			`Inside a Conversation, the other Agent will reply with another peer message — you'll see it as a "tool peer_send returned" line. ` +
			`Use peer_send to delegate a sub-step then await the reply, rather than trying to do everything yourself.` + "\n")
	}
	if containsAny(tools, []string{runners.GroupMemory}) {
		b.WriteString(`Tool: memory_put  — args {"scope": string, "key": string, "value": string}; scope="" is Room-private, scope="<volume>" is cross-Room` + "\n")
		b.WriteString(`Tool: memory_get  — args {"scope": string, "key": string}; returns {"exists":bool,"value":string}` + "\n")
		b.WriteString(`Tool: memory_list — args {"scope": string, "prefix": string}; returns {"keys":[string]}` + "\n")
		b.WriteString(`Tool: memory_delete — args {"scope": string, "key": string}` + "\n")
	}
	if containsAny(tools, []string{runners.GroupAITool}) {
		b.WriteString(`Tool: ai_tool_invoke — args {"tool": "claude-code", "prompt": string}; runs Claude Code CLI in the Room's /workspace dir, returns {"output": string}` + "\n")
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

// truncatePrefix returns up to n bytes of s, with an ellipsis when cut.
// Used for log lines so the wire / log file doesn't get blown up by the
// occasional long LLM reply.
func truncatePrefix(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
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
