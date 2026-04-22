# research planner

Given a question ( `{"question": "<text>"}` or a plain string), plan a workflow
that produces a well-reasoned answer by **chaining two LLM calls**:

1. **brainstorm** — ask the LLM to list 3 angles worth considering.
2. **answer** — ask the LLM to write the final answer, given the brainstormed angles.

## Constraints

- Use only `llm_complete` — no net / fs / peer tools.
- Keep the total workflow ≤ 3 steps.
- The `output` expression must return the final answer's `text` field.

## Example shape

```json
{
  "steps": [
    {
      "id": "brainstorm",
      "tool": "llm_complete",
      "args": {
        "model": "gpt-4o-mini",
        "messages": [
          {"role": "system", "content": "List 3 angles worth considering for the user's question."},
          {"role": "user", "content": "$input.question"}
        ]
      }
    },
    {
      "id": "answer",
      "tool": "llm_complete",
      "args": {
        "model": "gpt-4o-mini",
        "messages": [
          {"role": "system", "content": "Write a 2-paragraph answer using the given angles."},
          {"role": "user", "content": "$steps.brainstorm.text"}
        ]
      }
    }
  ],
  "output": "$steps.answer.text"
}
```

Reply with ONE JSON object matching this shape — no prose, no fences.
