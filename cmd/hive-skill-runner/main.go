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
	"sync"
	"time"

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

	// peer-router goroutine: owns a.Peers() consumption and routes each
	// message to either a registered awaiter (peer_call in flight) or
	// the fallback channel that drives runFromPeer.
	awaiter := newPeerAwaiter()
	go func() {
		for {
			select {
			case p, ok := <-a.Peers():
				if !ok {
					awaiter.Close()
					return
				}
				awaiter.Dispatch(p)
			case <-a.Done():
				awaiter.Close()
				return
			}
		}
	}()

	for {
		select {
		case task, ok := <-a.Tasks():
			if !ok {
				return
			}
			runOne(ctx, a, task, system, model, tools, awaiter)
		case peer, ok := <-awaiter.Fallback():
			if !ok {
				return
			}
			runFromPeer(ctx, a, peer, system, model, tools, awaiter)
		case <-a.Done():
			return
		}
	}
}

// runOne handles a top-level task/run dispatch. Final answer goes back
// via task/done; failures via task/error.
func runOne(ctx context.Context, a *hive.Agent, task *hive.Task, system, model string, tools []string, aw *peerAwaiter) {
	a.Log("info", "skill task received", map[string]any{"task_id": task.ID, "conv_id": task.ConvID})
	out, err := react(ctx, a, system, model, tools, string(task.Input), task.ConvID, aw)
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
func runFromPeer(ctx context.Context, a *hive.Agent, peer *hive.PeerMessage, system, model string, tools []string, aw *peerAwaiter) {
	if peer.ConvID == "" {
		a.Log("info", "peer message dropped (no conv_id)", map[string]any{"from": peer.From})
		return
	}
	a.Log("info", "skill peer received", map[string]any{"from": peer.From, "conv_id": peer.ConvID})

	out, err := react(ctx, a, system, model, tools, string(peer.Payload), peer.ConvID, aw)
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
// through so any peer_send / peer_call tool calls the LLM emits also
// count against the right Conversation transcript. aw is the peer-
// router's awaiter registry — peer_call uses it to block this react
// turn until the target replies.
func react(ctx context.Context, a *hive.Agent, system, model string, tools []string, userInput, convID string, aw *peerAwaiter) (map[string]any, error) {
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

		// peer_call is the synchronous variant: register an awaiter for
		// (to, conv_id), peer_send the message, then block until the
		// matching reply arrives or the timeout fires. The reply payload
		// becomes the tool result the LLM sees — same shape as fs_read /
		// llm_complete — so the LLM can synthesise the final answer
		// inclusive of the subordinate's actual output.
		//
		// Without this tool, peer-driven coordinator patterns leak the
		// reply outside the conversation transcript (timing-dependent;
		// see examples/paper-assistant/coordinator/README.md "v1
		// limitation" before this commit). peer_call closes that gap.
		if parsed.Tool == "peer_call" {
			result, terr := dispatchPeerCall(ctx, a, parsed.Args, convID, aw)
			if terr != nil {
				a.Log("error", "tool failed", map[string]any{"tool": "peer_call", "err": terr.Error()})
				msgs = append(msgs, hive.LLMMessage{
					Role:    "user",
					Content: fmt.Sprintf("tool peer_call failed: %s", terr.Error()),
				})
				continue
			}
			a.Log("info", "tool ok", map[string]any{"tool": "peer_call"})
			msgs = append(msgs, hive.LLMMessage{
				Role:    "user",
				Content: fmt.Sprintf("tool peer_call returned: %s", runners.ResultText(result, toolResultCap)),
			})
			continue
		}

		// peer_call_many is the parallel fan-out variant: register N
		// awaiters upfront (the awaiter registry is keyed by
		// (from, conv_id) — different `to` peers don't collide), then
		// spawn N goroutines that each PeerSend and await their reply.
		// Total wall-time = max(individual call), not sum. Returns one
		// result per call in the original order, so the LLM can match
		// reply i to call i without re-keying.
		//
		// Used when a coordinator needs feedback from multiple
		// subordinates (e.g. supervisor → reviewer-A + reviewer-B
		// concurrently) and wants both reviews in the transcript before
		// it produces a final aggregated answer.
		if parsed.Tool == "peer_call_many" {
			result, terr := dispatchPeerCallMany(ctx, a, parsed.Args, convID, aw)
			if terr != nil {
				a.Log("error", "tool failed", map[string]any{"tool": "peer_call_many", "err": terr.Error()})
				msgs = append(msgs, hive.LLMMessage{
					Role:    "user",
					Content: fmt.Sprintf("tool peer_call_many failed: %s", terr.Error()),
				})
				continue
			}
			a.Log("info", "tool ok", map[string]any{"tool": "peer_call_many"})
			msgs = append(msgs, hive.LLMMessage{
				Role:    "user",
				Content: fmt.Sprintf("tool peer_call_many returned: %s", runners.ResultText(result, toolResultCap)),
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
		b.WriteString(`Tool: peer_send — args {"to": string, "payload": any}; fire-and-forget message to another Agent in the same Room. Returns "sent" immediately. ` +
			`Use it when you don't need the other Agent's reply (one-way notify) or when subsequent flow is volume-mediated.` + "\n")
		b.WriteString(`Tool: peer_call — args {"to": string, "payload": any, "timeout_seconds"?: int}; SYNCHRONOUS request/reply variant. ` +
			`Sends the message AND waits for the target's peer_send reply (matched by from-name + conv_id), then returns the reply payload as the tool result so you can include it in your final answer. ` +
			`Default timeout 60s, max 300s. Only works inside a Conversation (needs conv_id to route the reply). ` +
			`This is the right tool for delegate-and-integrate patterns: hire_junior a worker, peer_call them with the task, weave their output into your answer.` + "\n")
		b.WriteString(`Tool: peer_call_many — args {"calls": [{"to": string, "payload": any}, ...], "timeout_seconds"?: int}; PARALLEL fan-out variant. ` +
			`Issues all peer_calls concurrently and returns once every reply (or timeout) is in. Wall-clock = max(individual reply time), not sum. ` +
			`Returns {"replies": [{"to": string, "ok": bool, "from"?: string, "payload"?: any, "error"?: string}]} in the same order as calls. ` +
			`Use it when you need feedback from N independent subordinates on the same artifact (e.g. supervisor → critic-A + critic-B reviewing the same draft).` + "\n")
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
	if containsAny(tools, []string{runners.GroupHire}) {
		b.WriteString(`Tool: hire_junior — args {"ref": "name:version", "rank": "intern|staff", "quota"?: {"tokens": {model: int}, "api_calls": {key: int}}, "volumes"?: [{"name","mode","mountpoint"}]}; ` +
			`spawns a subordinate Agent (manager+ rank only). Daemon enforces rank.CanHire (strictly lower) and atomically carves the quota out of your remaining budget. ` +
			`Returns {"image": string} — peer_send to that image to delegate work. Use this when a task needs a role that isn't already in the Room.` + "\n")
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

// peerCallTimeout is the per-call deadline for peer_call. Sized for a
// downstream Agent that needs to read the corpus, run an LLM completion,
// and write a section back — anything beyond a minute is almost
// certainly stuck and the caller should give up rather than block its
// own task indefinitely. Configurable per-call via args.timeout_seconds.
const peerCallTimeout = 60 * time.Second

// peerCallTimeoutCap bounds args.timeout_seconds; rejects unreasonable
// values that would let one stuck downstream peg the caller's task.
const peerCallTimeoutCap = 5 * time.Minute

// dispatchPeerCall implements the synchronous peer-call tool. The LLM
// requests it as: {"tool":"peer_call", "args":{"to":<image>,
// "payload":<any>, "timeout_seconds":<int, optional>}}. Returns
// {"from":<image>, "payload":<reply>} on success.
func dispatchPeerCall(ctx context.Context, a *hive.Agent, args map[string]any, convID string, aw *peerAwaiter) (any, error) {
	to, _ := args["to"].(string)
	if to == "" {
		return nil, fmt.Errorf("peer_call: to is required")
	}
	if convID == "" {
		// No conversation context — peer_call needs conv_id to route
		// the reply back. Surface a clear error instead of routing to
		// the fallback channel where the LLM never sees the result.
		return nil, fmt.Errorf("peer_call: no conv_id in scope; peer_call only works inside a Conversation")
	}
	timeout := peerCallTimeout
	if t, ok := args["timeout_seconds"].(float64); ok && t > 0 {
		d := time.Duration(t) * time.Second
		if d > peerCallTimeoutCap {
			d = peerCallTimeoutCap
		}
		timeout = d
	}

	ch, cancel := aw.Register(to, convID)
	defer cancel()

	// PeerSend after Register so the reply can never beat us — even
	// though delivery is asynchronous, registering first removes the
	// race. PeerSend itself is a sync IPC call to the daemon; if the
	// router rejects (peer not found, round_cap), the error returns
	// here and the awaiter is cancelled by defer.
	if err := a.PeerSend(ctx, to, args["payload"], hive.WithConv(convID)); err != nil {
		return nil, fmt.Errorf("peer_call: send failed: %w", err)
	}

	select {
	case reply, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("peer_call: awaiter cancelled before reply")
		}
		return map[string]any{
			"from":    reply.From,
			"payload": reply.Payload,
		}, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("peer_call: timeout after %s waiting for reply from %s", timeout, to)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// dispatchPeerCallMany implements parallel fan-out — peer_call to N
// peers concurrently, await all replies (or timeouts) in one shot.
// Args: {"calls": [{"to": <image>, "payload": <any>}, ...],
//        "timeout_seconds": <int, optional>}.
// Returns {"replies": [{"to": …, "ok": bool, "from"?, "payload"?, "error"?}]}
// in the original `calls` order so the LLM can pair replies to requests
// without keying.
func dispatchPeerCallMany(ctx context.Context, a *hive.Agent, args map[string]any, convID string, aw *peerAwaiter) (any, error) {
	if convID == "" {
		return nil, fmt.Errorf("peer_call_many: no conv_id in scope")
	}
	rawCalls, _ := args["calls"].([]any)
	if len(rawCalls) == 0 {
		return nil, fmt.Errorf("peer_call_many: calls is required and must be a non-empty list")
	}
	timeout := peerCallTimeout
	if t, ok := args["timeout_seconds"].(float64); ok && t > 0 {
		d := time.Duration(t) * time.Second
		if d > peerCallTimeoutCap {
			d = peerCallTimeoutCap
		}
		timeout = d
	}

	type call struct {
		to      string
		payload any
	}
	calls := make([]call, 0, len(rawCalls))
	for i, rc := range rawCalls {
		obj, ok := rc.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("peer_call_many: calls[%d] not an object", i)
		}
		to, _ := obj["to"].(string)
		if to == "" {
			return nil, fmt.Errorf("peer_call_many: calls[%d].to is required", i)
		}
		calls = append(calls, call{to: to, payload: obj["payload"]})
	}

	// Register all awaiters synchronously BEFORE any send — guarantees
	// no reply can race past its own awaiter. The peer-router goroutine
	// is concurrent with this code, but Register is fast (one mutex op).
	chans := make([]<-chan *hive.PeerMessage, len(calls))
	cancels := make([]func(), len(calls))
	for i, c := range calls {
		chans[i], cancels[i] = aw.Register(c.to, convID)
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	// Fan out: one goroutine per call. Results indexed by i so we
	// preserve the request order in the response (a tool-result map
	// with stable indices is much easier for the LLM to reason about).
	type result struct {
		To      string `json:"to"`
		OK      bool   `json:"ok"`
		From    string `json:"from,omitempty"`
		Payload any    `json:"payload,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]result, len(calls))
	var wg sync.WaitGroup
	deadline := time.After(timeout)
	for i := range calls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i].To = calls[i].to
			if err := a.PeerSend(ctx, calls[i].to, calls[i].payload, hive.WithConv(convID)); err != nil {
				results[i].Error = "send failed: " + err.Error()
				return
			}
			select {
			case reply, ok := <-chans[i]:
				if !ok || reply == nil {
					results[i].Error = "awaiter cancelled before reply"
					return
				}
				results[i].OK = true
				results[i].From = reply.From
				// Decode payload back into a generic value so the LLM
				// sees structured data, not a raw JSON string.
				var p any
				if err := json.Unmarshal(reply.Payload, &p); err == nil {
					results[i].Payload = p
				} else {
					results[i].Payload = string(reply.Payload)
				}
			case <-deadline:
				results[i].Error = fmt.Sprintf("timeout after %s waiting for reply from %s", timeout, calls[i].to)
			case <-ctx.Done():
				results[i].Error = ctx.Err().Error()
			}
		}(i)
	}
	wg.Wait()
	return map[string]any{"replies": results}, nil
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
