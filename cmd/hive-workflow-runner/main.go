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
	for task := range a.Tasks() {
		runOne(ctx, a, task, mode, model, tools)
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

func runOne(ctx context.Context, a *hive.Agent, task *hive.Task, mode *runMode, model string, tools []string) {
	a.Log("info", "workflow task received", map[string]any{"task_id": task.ID, "mode": mode.name})

	wf := mode.static
	if mode.name == "llm" {
		planned, err := planWorkflow(ctx, a, mode.prompt, model, tools, task.Input)
		if err != nil {
			a.Log("error", "planner failed", map[string]any{"err": err.Error()})
			_ = task.Fail(2, err.Error())
			return
		}
		wf = planned
		a.Log("info", "planner produced workflow", map[string]any{"steps": len(wf.Steps)})
	}

	// Unmarshal task input into a generic value so `$input.<path>` works.
	// Non-JSON inputs become a plain string — users then reference via
	// `$input` as a whole, not `$input.something`.
	var input any
	if len(task.Input) > 0 {
		if err := json.Unmarshal(task.Input, &input); err != nil {
			input = string(task.Input)
		}
	}

	wctx := workflow.NewContext(input)
	for i, step := range wf.Steps {
		if !runners.ToolAllowed(step.Tool, tools) {
			_ = task.Fail(3, fmt.Sprintf("step[%d] %s: tool %q not in allow-list %v", i, step.ID, step.Tool, tools))
			return
		}
		resolvedRaw, err := workflow.Resolve(step.Args, wctx)
		if err != nil {
			_ = task.Fail(4, fmt.Sprintf("step[%d] %s: %v", i, step.ID, err))
			return
		}
		resolvedArgs, _ := resolvedRaw.(map[string]any)
		if resolvedArgs == nil {
			resolvedArgs = map[string]any{}
		}
		a.Log("info", "step start", map[string]any{"i": i, "id": step.ID, "tool": step.Tool})
		result, err := runners.DispatchTool(ctx, a, step.Tool, resolvedArgs)
		if err != nil {
			_ = task.Fail(5, fmt.Sprintf("step[%d] %s (%s): %v", i, step.ID, step.Tool, err))
			return
		}
		wctx.Steps[step.ID] = result
	}

	out, err := wf.ResolveOutput(wctx)
	if err != nil {
		_ = task.Fail(6, fmt.Sprintf("resolve output: %v", err))
		return
	}
	_ = task.Reply(map[string]any{
		"output": out,
		"mode":   mode.name,
		"steps":  wctx.Steps,
	})
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
		b.WriteString(`  peer_send    args {to,payload}` + "\n")
	}
	if hasTool(tools, runners.GroupLLM) {
		b.WriteString(`  llm_complete args {model,messages,max_tokens?} → {text,usage}` + "\n")
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
