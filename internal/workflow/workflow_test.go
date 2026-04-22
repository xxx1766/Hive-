package workflow

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse_Happy(t *testing.T) {
	raw := []byte(`{
		"description": "d",
		"steps": [
			{"id":"a","tool":"net_fetch","args":{"url":"$input.url"}},
			{"id":"b","tool":"fs_write","args":{"path":"/tmp/x","content":"$steps.a.body"}}
		],
		"output":"$steps.b"
	}`)
	wf, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(wf.Steps) != 2 || wf.Output != "$steps.b" {
		t.Fatalf("bad parse: %+v", wf)
	}
}

func TestValidate_EmptySteps(t *testing.T) {
	wf := &Workflow{}
	if err := wf.Validate(); err == nil {
		t.Fatal("expected error for empty steps")
	}
}

func TestValidate_DuplicateID(t *testing.T) {
	wf := &Workflow{Steps: []Step{
		{ID: "a", Tool: "net_fetch"},
		{ID: "a", Tool: "fs_read"},
	}}
	if err := wf.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected duplicate-id error, got %v", err)
	}
}

func TestValidate_MissingTool(t *testing.T) {
	wf := &Workflow{Steps: []Step{{ID: "a"}}}
	if err := wf.Validate(); err == nil {
		t.Fatal("expected missing-tool error")
	}
}

func TestResolve_InputRef(t *testing.T) {
	ctx := NewContext(map[string]any{"url": "https://example.com"})
	got, err := Resolve("$input.url", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://example.com" {
		t.Fatalf("got %v", got)
	}
}

func TestResolve_StepRef(t *testing.T) {
	ctx := NewContext(nil)
	ctx.Steps["fetch"] = map[string]any{"body": "<html>...", "status": 200}

	got, err := Resolve("$steps.fetch.body", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "<html>..." {
		t.Fatalf("got %v", got)
	}

	got, err = Resolve("$steps.fetch.status", ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != 200 {
		t.Fatalf("type preservation: got %T %v", got, got)
	}
}

func TestResolve_NestedArgs(t *testing.T) {
	ctx := NewContext(map[string]any{"url": "U"})
	args := map[string]any{
		"method": "GET",
		"url":    "$input.url",
		"headers": map[string]any{
			"Authorization": "$input.url", // deliberate reuse
		},
	}
	out, err := Resolve(args, ctx)
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["url"] != "U" || m["method"] != "GET" {
		t.Fatalf("flat substitution failed: %+v", m)
	}
	if m["headers"].(map[string]any)["Authorization"] != "U" {
		t.Fatalf("nested substitution failed: %+v", m)
	}
}

func TestResolve_Slice(t *testing.T) {
	ctx := NewContext(map[string]any{"a": 1, "b": 2})
	out, err := Resolve([]any{"$input.a", "$input.b", "plain"}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []any{1, 2, "plain"}
	if !reflect.DeepEqual(out, want) {
		t.Fatalf("got %+v, want %+v", out, want)
	}
}

func TestResolve_MissingKey(t *testing.T) {
	ctx := NewContext(map[string]any{"a": 1})
	if _, err := Resolve("$input.missing", ctx); err == nil {
		t.Fatal("expected missing-key error")
	}
}

func TestResolve_UnknownRoot(t *testing.T) {
	ctx := NewContext(nil)
	if _, err := Resolve("$something.else", ctx); err == nil {
		t.Fatal("expected unknown-root error")
	}
}

func TestResolve_PassThroughLiterals(t *testing.T) {
	ctx := NewContext(nil)
	for _, v := range []any{42, true, 3.14, nil, "plain"} {
		got, err := Resolve(v, ctx)
		if err != nil {
			t.Fatalf("Resolve(%v): %v", v, err)
		}
		if got != v {
			t.Errorf("Resolve(%v): got %v", v, got)
		}
	}
}

func TestResolveOutput_EmptyUsesLastStep(t *testing.T) {
	wf := &Workflow{Steps: []Step{
		{ID: "a", Tool: "net_fetch"},
		{ID: "b", Tool: "fs_write"},
	}}
	ctx := NewContext(nil)
	ctx.Steps["a"] = "first"
	ctx.Steps["b"] = "second"
	got, err := wf.ResolveOutput(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "second" {
		t.Fatalf("expected last step's result, got %v", got)
	}
}

func TestResolveOutput_Expression(t *testing.T) {
	wf := &Workflow{
		Steps:  []Step{{ID: "a", Tool: "net_fetch"}},
		Output: "$steps.a.body",
	}
	ctx := NewContext(nil)
	ctx.Steps["a"] = map[string]any{"body": "hello"}
	got, err := wf.ResolveOutput(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != "hello" {
		t.Fatalf("got %v", got)
	}
}
