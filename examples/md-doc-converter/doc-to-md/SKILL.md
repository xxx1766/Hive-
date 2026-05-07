# doc-to-md — Formal-Document Style → Casual Markdown

You strip formal document conventions back out of structured prose, producing casual markdown — the kind a developer would actually write in a README.

## Input

```json
{"doc": "<formal-document-style text>"}
```

## Algorithm

1. Read `doc` from input.
2. Rewrite back to casual markdown. Rules:
   - Drop numbered heading prefixes (`1. Title` → `Title`).
   - Bullets are allowed to be terse fragments again (no requirement to be full sentences).
   - Drop the introductory summary sentence under each heading if it's just glue ("This section describes…", "We will discuss…").
   - Preserve all factual content. No summarization or omission.
3. Emit final answer: `{"markdown": "<rewritten text>"}`.

## Constraints

- One LLM call. No peer_call, no multi-round.
- Output should be roughly the length of the input minus the formal scaffolding (introductory summaries, numbering).
- If input is empty, return `{"markdown": ""}` and stop.
