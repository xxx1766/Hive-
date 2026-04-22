// summarize is a staff-rank Agent that calls Hive's LLM proxy to reduce
// arbitrary text to a short summary. Demonstrates:
//   - Rank=staff having LLMAllowed
//   - per-Agent token quota being deducted on each call
//   - the "provider" field defaulting to whatever the daemon picked
//     (openai if OPENAI_API_KEY is set, else mock)
package main

import (
	"context"
	"encoding/json"

	hive "github.com/anne-x/hive/sdk/go"
)

type input struct {
	Text string `json:"text"`
}

type output struct {
	Summary string `json:"summary"`
	Usage   struct {
		Prompt, Completion, Total int
	} `json:"usage"`
}

func main() {
	a := hive.MustConnect()
	defer a.Close()

	ctx := context.Background()

	for task := range a.Tasks() {
		var in input
		if err := json.Unmarshal(task.Input, &in); err != nil {
			task.Fail(1, "bad input: "+err.Error())
			continue
		}
		if in.Text == "" {
			task.Fail(1, "input.text is required")
			continue
		}
		a.Log("info", "summarizing", map[string]any{"bytes": len(in.Text)})

		text, usage, err := a.LLMComplete(ctx, "", "gpt-4o-mini", []hive.LLMMessage{
			{Role: "system", Content: "Summarise the user's text in one sentence."},
			{Role: "user", Content: in.Text},
		}, 128)
		if err != nil {
			a.Log("error", "llm failed", map[string]any{"err": err.Error()})
			task.Fail(2, err.Error())
			continue
		}

		out := output{Summary: text}
		out.Usage.Prompt = usage.PromptTokens
		out.Usage.Completion = usage.CompletionTokens
		out.Usage.Total = usage.TotalTokens
		task.Reply(out)
	}
}
