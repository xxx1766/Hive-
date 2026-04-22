// Package workflow defines the declarative Agent "flow.json" format
// consumed by `kind: workflow` Agents, plus the variable-substitution
// engine that resolves `$input.x` and `$steps.<id>.<path>` references
// before a step executes.
//
// Design notes:
//
//   - Workflow is executed sequentially (MVP) — no branching / retries.
//     A `when:` gate and `on_error:` policy are future additions that
//     won't break the current JSON schema because the fields are optional.
//
//   - Variable substitution is "whole-value" only: a string that equals
//     exactly "$input.url" is replaced with the referenced value and
//     preserves its original type. Strings like "prefix $input.x" are
//     left alone in MVP — adding sprintf-style interpolation later is
//     purely additive.
//
//   - Execution itself lives in cmd/hive-workflow-runner (it needs
//     *hive.Agent, which would pull the SDK into this package). Here
//     we own the schema, validation, and substitution.
package workflow

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Workflow is what flow.json / an LLM planner produces.
type Workflow struct {
	Description string `json:"description,omitempty"`
	Steps       []Step `json:"steps"`
	// Output is an optional expression ("$steps.<id>.<field>" or "$input.<field>")
	// evaluated after all steps finish. If empty, the last step's whole result
	// is returned.
	Output string `json:"output,omitempty"`
}

// Step is a single tool invocation.
type Step struct {
	ID   string         `json:"id"`
	Tool string         `json:"tool"`
	Args map[string]any `json:"args,omitempty"`
}

// Parse unmarshals a JSON-encoded Workflow and validates its basic shape.
func Parse(raw []byte) (*Workflow, error) {
	var wf Workflow
	if err := json.Unmarshal(raw, &wf); err != nil {
		return nil, fmt.Errorf("workflow: parse JSON: %w", err)
	}
	if err := wf.Validate(); err != nil {
		return nil, err
	}
	return &wf, nil
}

// Validate checks the required fields and unique step IDs. We intentionally
// do NOT validate that `$steps.X.Y` references exist in prior steps — that
// shows up as a clean runtime error in Resolve.
func (wf *Workflow) Validate() error {
	if len(wf.Steps) == 0 {
		return fmt.Errorf("workflow: steps must not be empty")
	}
	seen := make(map[string]bool, len(wf.Steps))
	for i, s := range wf.Steps {
		if s.ID == "" {
			return fmt.Errorf("workflow: steps[%d].id is required", i)
		}
		if seen[s.ID] {
			return fmt.Errorf("workflow: duplicate step id %q", s.ID)
		}
		seen[s.ID] = true
		if s.Tool == "" {
			return fmt.Errorf("workflow: steps[%d] (id=%s): tool is required", i, s.ID)
		}
	}
	return nil
}

// Context carries the data accessible to `$...` references during Resolve.
// `Input` is the task's JSON payload; `Steps` accumulates each step's
// result keyed by step ID.
type Context struct {
	Input any
	Steps map[string]any
}

// NewContext initialises a fresh context for a workflow run.
func NewContext(input any) *Context {
	return &Context{Input: input, Steps: map[string]any{}}
}

// Resolve walks v recursively, replacing any string that starts with '$' and
// follows the "$input.<path>" or "$steps.<id>.<path>" grammar with the
// referenced value.
//
// Types:
//   - map[string]any / []any: recursed into
//   - string: may be replaced (whole-string match only)
//   - everything else: passed through untouched
//
// If a reference can't be resolved, Resolve returns an error carrying the
// offending expression — callers surface it so the user can fix flow.json.
func Resolve(v any, ctx *Context) (any, error) {
	switch t := v.(type) {
	case string:
		if strings.HasPrefix(t, "$") {
			return lookup(t, ctx)
		}
		return t, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, sub := range t {
			resolved, err := Resolve(sub, ctx)
			if err != nil {
				return nil, fmt.Errorf("in %q: %w", k, err)
			}
			out[k] = resolved
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, sub := range t {
			resolved, err := Resolve(sub, ctx)
			if err != nil {
				return nil, fmt.Errorf("in [%d]: %w", i, err)
			}
			out[i] = resolved
		}
		return out, nil
	default:
		return v, nil
	}
}

// ResolveOutput evaluates the optional Output expression after execution.
// If Output is empty, returns the last step's result (or nil if no steps).
func (wf *Workflow) ResolveOutput(ctx *Context) (any, error) {
	if wf.Output == "" {
		if len(wf.Steps) == 0 {
			return nil, nil
		}
		return ctx.Steps[wf.Steps[len(wf.Steps)-1].ID], nil
	}
	return Resolve(wf.Output, ctx)
}

// lookup interprets a single "$..." reference.
func lookup(ref string, ctx *Context) (any, error) {
	parts := strings.Split(strings.TrimPrefix(ref, "$"), ".")
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty reference %q", ref)
	}
	var root any
	switch parts[0] {
	case "input":
		root = ctx.Input
	case "steps":
		root = ctx.Steps
	default:
		return nil, fmt.Errorf("unknown reference root %q (want $input or $steps)", parts[0])
	}
	return walkPath(ref, root, parts[1:])
}

// walkPath traverses object paths. Accepts both map[string]any and the
// less-common generic map[any]any that yaml.v3 sometimes produces.
func walkPath(fullRef string, root any, path []string) (any, error) {
	cur := root
	for _, key := range path {
		switch t := cur.(type) {
		case map[string]any:
			next, ok := t[key]
			if !ok {
				return nil, fmt.Errorf("reference %q: key %q not found", fullRef, key)
			}
			cur = next
		case map[any]any:
			next, ok := t[key]
			if !ok {
				return nil, fmt.Errorf("reference %q: key %q not found", fullRef, key)
			}
			cur = next
		case nil:
			return nil, fmt.Errorf("reference %q: got nil before reaching %q", fullRef, key)
		default:
			return nil, fmt.Errorf("reference %q: cannot descend into %T at %q", fullRef, cur, key)
		}
	}
	return cur, nil
}
