// hive-workflow-runner is Hive's built-in executor for `kind: workflow`
// Agents. Two modes, same binary:
//
//	STATIC:  HIVE_WORKFLOW_PATH points at flow.json → load once, execute per task.
//	LLM:     HIVE_PLANNER_PATH points at PLANNER.md → for each task, ask the
//	         LLM to emit a flow.json, validate, then execute.
//
// Variable resolution and step execution are identical in both modes —
// the only difference is where the Workflow comes from. Ranks and quotas
// are enforced by the daemon's proxy layer as usual; the allow-list in
// manifest.tools is a second gate applied here (so the planner isn't
// tempted to emit a tool the Agent can't legally dispatch).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/anne-x/hive/internal/peerawait"
	"github.com/anne-x/hive/internal/runners"
	"github.com/anne-x/hive/internal/workflow"
	hive "github.com/anne-x/hive/sdk/go"
)

const (
	maxPlannerAttempts = 3
	plannerMaxTokens   = 1024
)

func main() {
	a := hive.MustConnect()
	defer a.Close()

	mode, err := detectMode()
	if err != nil {
		a.Log("error", "workflow runner init", map[string]any{"err": err.Error()})
		os.Exit(2)
	}

	model := os.Getenv("HIVE_MODEL")
	if model == "" {
		model = "gpt-4o-mini"
	}
	tools := parseCSV(os.Getenv("HIVE_TOOLS"))
	if len(tools) == 0 {
		tools = []string{runners.GroupNet, runners.GroupFS, runners.GroupPeer, runners.GroupLLM}
	}

	a.Log("info", "workflow runner ready", map[string]any{
		"mode":  mode.name,
		"model": model,
		"tools": tools,
	})

	ctx := context.Background()

	// peer-router goroutine: owns a.Peers() consumption and routes each
	// inbound message to either a registered awaiter (peer_call in
	// flight from a workflow step) or the fallback channel that drives
	// runFromPeer (ad-hoc inbound peer with conv_id treated as a new
	// task trigger). Same architecture as hive-skill-runner; lets a
	// kind: workflow agent be both a peer_call initiator (via flow.json)
	// and a peer_call target.
	aw := peerawait.New()
	go func() {
		for {
			select {
			case p, ok := <-a.Peers():
				if !ok {
					aw.Close()
					return
				}
				aw.Dispatch(p)
			case <-a.Done():
				aw.Close()
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
			runOne(ctx, a, task, mode, model, tools, aw)
		case peer, ok := <-aw.Fallback():
			if !ok {
				return
			}
			runFromPeer(ctx, a, peer, mode, model, tools, aw)
		case <-a.Done():
			return
		}
	}
}

// runFromPeer treats an inbound peer/recv with a non-empty ConvID as a
// follow-up workflow execution. Same pipeline as runOne, but the result
// is shipped back to the sender as a peer/send hop within the same
// Conversation. Peer messages without ConvID are dropped (logged) since
// workflows have no contract for ad-hoc inbound chat.
func runFromPeer(ctx context.Context, a *hive.Agent, peer *hive.PeerMessage, mode *runMode, model string, tools []string, aw *peerawait.Awaiter) {
	if peer.ConvID == "" {
		a.Log("info", "peer message dropped (no conv_id)", map[string]any{"from": peer.From})
		return
	}
	a.Log("info", "workflow peer received", map[string]any{"from": peer.From, "conv_id": peer.ConvID, "mode": mode.name})

	out, err := executeWorkflow(ctx, a, mode, model, tools, peer.Payload, peer.ConvID, aw)
	if err != nil {
		_ = a.PeerSend(ctx, peer.From, map[string]any{"error": err.Error()}, hive.WithConv(peer.ConvID))
		return
	}
	if err := a.PeerSend(ctx, peer.From, out, hive.WithConv(peer.ConvID)); err != nil {
		a.Log("error", "peer reply failed", map[string]any{"err": err.Error(), "conv_id": peer.ConvID})
	}
}

// ── mode selection ───────────────────────────────────────────────────────

type runMode struct {
	name   string // "static" or "llm"
	static *workflow.Workflow
	prompt string // for llm mode
}

func detectMode() (*runMode, error) {
	if p := os.Getenv("HIVE_WORKFLOW_PATH"); p != "" {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read workflow %s: %w", p, err)
		}
		wf, err := workflow.Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("parse workflow: %w", err)
		}
		return &runMode{name: "static", static: wf}, nil
	}
	if p := os.Getenv("HIVE_PLANNER_PATH"); p != "" {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read planner %s: %w", p, err)
		}
		return &runMode{name: "llm", prompt: string(raw)}, nil
	}
	return nil, fmt.Errorf("set either HIVE_WORKFLOW_PATH (static) or HIVE_PLANNER_PATH (LLM)")
}

// ── task execution ───────────────────────────────────────────────────────

func runOne(ctx context.Context, a *hive.Agent, task *hive.Task, mode *runMode, model string, tools []string, aw *peerawait.Awaiter) {
	a.Log("info", "workflow task received", map[string]any{"task_id": task.ID, "mode": mode.name, "conv_id": task.ConvID})
	out, err := executeWorkflow(ctx, a, mode, model, tools, task.Input, task.ConvID, aw)
	if err != nil {
		// executeWorkflow returns a *runErr for staged failures so we can
		// preserve the original exit code — keep the existing 2..6 mapping.
		if re, ok := err.(*runErr); ok {
			_ = task.Fail(re.code, re.msg)
			return
		}
		_ = task.Fail(2, err.Error())
		return
	}
	_ = task.Reply(out)
}

// runErr is a typed error so executeWorkflow can carry the legacy exit
// codes (2: planner, 3: tool not allowed, 4: arg resolve, 5: tool, 6:
// output) through to the task/error caller without losing semantics.
type runErr struct {
	code int
	msg  string
}

func (e *runErr) Error() string { return e.msg }

// executeWorkflow runs the configured mode against `input` and returns
// the structured reply (`{output, mode, steps}`). convID, when non-empty,
// is injected into peer_send args so cross-Agent hops emitted by the
// workflow contribute to the right Conversation transcript.
func executeWorkflow(ctx context.Context, a *hive.Agent, mode *runMode, model string, tools []string, input json.RawMessage, convID string, aw *peerawait.Awaiter) (map[string]any, error) {
	wf := mode.static
	if mode.name == "llm" {
		planned, err := planWorkflow(ctx, a, mode.prompt, model, tools, input)
		if err != nil {
			a.Log("error", "planner failed", map[string]any{"err": err.Error()})
			return nil, &runErr{code: 2, msg: err.Error()}
		}
		wf = planned
		a.Log("info", "planner produced workflow", map[string]any{"steps": len(wf.Steps)})
	}

	// Unmarshal task input into a generic value so `$input.<path>` works.
	// Non-JSON inputs become a plain string — users then reference via
	// `$input` as a whole, not `$input.something`.
	var inputAny any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &inputAny); err != nil {
			inputAny = string(input)
		}
	}

	wctx := workflow.NewContext(inputAny)
	for i, step := range wf.Steps {
		if !runners.ToolAllowed(step.Tool, tools) {
			return nil, &runErr{code: 3, msg: fmt.Sprintf("step[%d] %s: tool %q not in allow-list %v", i, step.ID, step.Tool, tools)}
		}
		resolvedRaw, err := workflow.Resolve(step.Args, wctx)
		if err != nil {
			return nil, &runErr{code: 4, msg: fmt.Sprintf("step[%d] %s: %v", i, step.ID, err)}
		}
		resolvedArgs, _ := resolvedRaw.(map[string]any)
		if resolvedArgs == nil {
			resolvedArgs = map[string]any{}
		}
		// LLM model fallback: when an llm_complete step doesn't pin a
		// model in flow.json, use HIVE_MODEL (set by the daemon from the
		// manifest's `model:` field, possibly overridden by `hive hire
		// --model X`). Mirrors skill-runner's HIVE_MODEL fallback so
		// workflow agents pick up `--model` too without editing flow.json.
		if step.Tool == "llm_complete" {
			if m, ok := resolvedArgs["model"]; !ok || m == nil || m == "" {
				if envModel := os.Getenv("HIVE_MODEL"); envModel != "" {
					resolvedArgs["model"] = envModel
				}
			}
		}
		// Conversation hop attribution: a workflow step that emits a
		// peer hop inside a Conversation should advance the right
		// transcript. peer_call uses the conv_id passed to its
		// dispatcher (separate parameter, not args), so injection only
		// matters for peer_send here. Explicit > implicit.
		if step.Tool == "peer_send" && convID != "" {
			if v, ok := resolvedArgs["conv_id"]; !ok || v == nil || v == "" {
				resolvedArgs["conv_id"] = convID
			}
		}
		a.Log("info", "step start", map[string]any{"i": i, "id": step.ID, "tool": step.Tool})
		var (
			stepResult any
			stepErr    error
		)
		switch step.Tool {
		case "peer_call":
			// Synchronous request/reply — register awaiter, PeerSend,
			// block on the matching reply. Result shape:
			// {"from": <name>, "payload": <reply>}. Stored in
			// wctx.Steps[step.ID]; downstream steps reference it as
			// $steps.<id>.payload.
			stepResult, stepErr = dispatchPeerCall(ctx, a, resolvedArgs, convID, aw)
		case "peer_call_many":
			// Parallel fan-out — one PeerSend per call, all awaiters
			// registered upfront, WaitGroup-join. Result shape:
			// {"replies": [{"to","ok","from","payload"|"error"}, ...]}
			// in the original calls order.
			stepResult, stepErr = dispatchPeerCallMany(ctx, a, resolvedArgs, convID, aw)
		default:
			stepResult, stepErr = runners.DispatchTool(ctx, a, step.Tool, resolvedArgs)
		}
		if stepErr != nil {
			return nil, &runErr{code: 5, msg: fmt.Sprintf("step[%d] %s (%s): %v", i, step.ID, step.Tool, stepErr)}
		}
		wctx.Steps[step.ID] = stepResult
	}

	out, err := wf.ResolveOutput(wctx)
	if err != nil {
		return nil, &runErr{code: 6, msg: fmt.Sprintf("resolve output: %v", err)}
	}
	return map[string]any{
		"output": out,
		"mode":   mode.name,
		"steps":  wctx.Steps,
	}, nil
}

// ── LLM planner (kind=workflow, planner: mode) ───────────────────────────

// planWorkflow asks the LLM to produce a flow.json. Up to maxPlannerAttempts
// tries with error feedback between attempts — LLMs sometimes wrap JSON in
// prose or fence it; the regex fallback handles the common cases, the
// retry with an explicit error message handles the rare ones.
func planWorkflow(ctx context.Context, a *hive.Agent, planner, model string, tools []string, taskInput json.RawMessage) (*workflow.Workflow, error) {
	system := planner + "\n\n" + plannerInstructions(tools)
	msgs := []hive.LLMMessage{
		{Role: "system", Content: system},
		{Role: "user", Content: string(taskInput)},
	}
	var lastErr error
	for attempt := 1; attempt <= maxPlannerAttempts; attempt++ {
		text, _, err := a.LLMComplete(ctx, "", model, msgs, plannerMaxTokens)
		if err != nil {
			return nil, fmt.Errorf("llm attempt %d: %w", attempt, err)
		}
		wf, perr := parsePlannerReply(text)
		if perr == nil {
			return wf, nil
		}
		lastErr = perr
		msgs = append(msgs,
			hive.LLMMessage{Role: "assistant", Content: text},
			hive.LLMMessage{Role: "user", Content: fmt.Sprintf(
				"Your JSON was invalid (%v). Reply again with ONLY a valid workflow object, no prose, no fences.", perr)},
		)
	}
	return nil, fmt.Errorf("planner failed after %d attempts: %w", maxPlannerAttempts, lastErr)
}

var jsonObjRe = regexp.MustCompile(`\{(?s).*\}`)

func parsePlannerReply(text string) (*workflow.Workflow, error) {
	trimmed := strings.TrimSpace(text)
	if wf, err := workflow.Parse([]byte(trimmed)); err == nil {
		return wf, nil
	}
	if m := jsonObjRe.FindString(trimmed); m != "" {
		return workflow.Parse([]byte(m))
	}
	return nil, fmt.Errorf("no parseable JSON in planner reply")
}

func plannerInstructions(tools []string) string {
	var b strings.Builder
	b.WriteString("You are producing a Hive workflow. Respond with ONLY a JSON object matching this schema:\n")
	b.WriteString(`  {"steps":[{"id":"<str>","tool":"<str>","args":{...}}...],"output":"$steps.<id>.<field>"}` + "\n\n")
	b.WriteString("Values in args may reference:\n")
	b.WriteString("  $input.<path>       — the task's JSON input\n")
	b.WriteString("  $steps.<id>.<path>  — a previous step's result\n\n")
	b.WriteString("Available tools (gated by manifest allow-list):\n")
	if hasTool(tools, runners.GroupNet) {
		b.WriteString(`  net_fetch    args {url,method?,headers?,body?} → {status,body}` + "\n")
	}
	if hasTool(tools, runners.GroupFS) {
		b.WriteString(`  fs_read      args {path} → string` + "\n")
		b.WriteString(`  fs_write     args {path,content} → "ok"` + "\n")
		b.WriteString(`  fs_list      args {path} → [{name,is_dir,size}...]` + "\n")
	}
	if hasTool(tools, runners.GroupPeer) {
		b.WriteString(`  peer_send       args {to,payload} → "sent"  (fire-and-forget; returns immediately)` + "\n")
		b.WriteString(`  peer_call       args {to,payload,timeout_seconds?} → {from,payload}  (sync await; only inside a Conversation)` + "\n")
		b.WriteString(`  peer_call_many  args {calls:[{to,payload},...],timeout_seconds?} → {replies:[{to,ok,from,payload,error},...]}  (parallel fan-out)` + "\n")
	}
	if hasTool(tools, runners.GroupLLM) {
		b.WriteString(`  llm_complete args {model,messages,max_tokens?} → {text,usage}` + "\n")
	}
	if hasTool(tools, runners.GroupMemory) {
		b.WriteString(`  memory_put    args {scope,key,value} → "ok"  (scope="" private, "<vol>" shared)` + "\n")
		b.WriteString(`  memory_get    args {scope,key} → {exists,value}` + "\n")
		b.WriteString(`  memory_list   args {scope,prefix} → {keys}` + "\n")
		b.WriteString(`  memory_delete args {scope,key} → "ok"` + "\n")
	}
	if hasTool(tools, runners.GroupAITool) {
		b.WriteString(`  ai_tool_invoke args {tool,prompt} → {output}   (runs Claude Code CLI with cwd=/workspace)` + "\n")
	}
	b.WriteString("\nDo NOT wrap the JSON in markdown fences. Return the bare object.\n")
	return b.String()
}

// ── tiny helpers ─────────────────────────────────────────────────────────

func parseCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func hasTool(list []string, want string) bool {
	for _, t := range list {
		if t == want {
			return true
		}
	}
	return false
}
