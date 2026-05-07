package main

import (
	"reflect"
	"strings"
	"testing"
)

// The LLM-reply parser is the only unit-testable logic that stayed in the
// skill runner after extracting dispatch/group helpers to internal/runners.
// Tool-level tests live in internal/runners/tools_test.go.

func TestParseReply_DirectJSON(t *testing.T) {
	r := parseReply(`{"answer":"done"}`)
	if r.Answer != "done" {
		t.Fatalf("want answer=done, got %+v", r)
	}
}

func TestParseReply_ToolCall(t *testing.T) {
	r := parseReply(`{"tool":"net_fetch","args":{"url":"https://x"}}`)
	if r.Tool != "net_fetch" || r.Args["url"] != "https://x" {
		t.Fatalf("unexpected parse: %+v", r)
	}
}

func TestParseReply_FencedJSON(t *testing.T) {
	// LLMs often wrap JSON in triple-backtick fences with prose.
	in := "Here is my call:\n\n```json\n{\"tool\":\"fs_read\",\"args\":{\"path\":\"/data/x\"}}\n```\n"
	r := parseReply(in)
	if r.Tool != "fs_read" {
		t.Fatalf("fenced extraction failed: %+v", r)
	}
}

func TestParseReply_PlainText_EmptyResult(t *testing.T) {
	// Plain text (e.g. mock provider's "mock-summary: ...") must yield
	// an empty reply so the caller can apply the fallback "treat as answer".
	r := parseReply("mock-summary: hello world")
	if r.Tool != "" || r.Answer != "" {
		t.Fatalf("non-JSON should parse to zero reply, got %+v", r)
	}
}

func TestParseReply_IrrelevantJSONObject_Rejected(t *testing.T) {
	// An object that has neither "tool" nor "answer" must not masquerade
	// as a valid reply.
	r := parseReply(`{"some":"other"}`)
	if r.Tool != "" || r.Answer != "" {
		t.Fatalf("unrelated object should not parse as reply, got %+v", r)
	}
}

// TestParseReply_MultipleObjects covers the case some hosted LLMs
// (notably openai/gpt-5.4-mini through GMI) emit when they try to
// "plan ahead" by chaining several tool calls in one reply. The
// runner does one tool call per turn, so we pick the first usable
// object and discard the rest.
func TestParseReply_MultipleObjects(t *testing.T) {
	in := `{"tool":"fs_read","args":{"path":"/a"}}
{"tool":"fs_read","args":{"path":"/b"}}
{"answer":"done"}`
	r := parseReply(in)
	if r.Tool != "fs_read" {
		t.Fatalf("first tool call not picked: %+v", r)
	}
	if got := r.Args["path"]; got != "/a" {
		t.Errorf("first object's args lost: %v", got)
	}
}

// TestParseReply_MultipleObjects_FirstNoShape covers the variant where
// the model emits a speculative {"thought":"…"} or similar prefix that
// has no usable fields, then a real tool call. We must skip the prefix
// rather than returning an empty reply.
func TestParseReply_MultipleObjects_FirstNoShape(t *testing.T) {
	in := `{"thought":"let me first read the corpus"} {"tool":"peer_send","args":{"to":"x","payload":1}}`
	r := parseReply(in)
	if r.Tool != "peer_send" {
		t.Fatalf("expected peer_send, got %+v", r)
	}
}

// TestTopLevelObjects exercises the brace-counting scanner directly so
// future edits don't accidentally regress the depth-tracking edges.
// Strings with quoted braces / escaped quotes are the common gotchas.
func TestTopLevelObjects(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"no objects here", 0},
		{`{"a":1}`, 1},
		{`{"a":1}{"b":2}`, 2},
		{"{\"a\":1}\n{\"b\":2}", 2},
		{`{"a":{"b":1}}`, 1},
		{`{"a":"} not a closer"}`, 1},
		{`{"a":"he said \"hi\" "}`, 1},
		{`{"a":1`, 0}, // unbalanced
	}
	for _, c := range cases {
		got := topLevelObjects(c.in)
		if len(got) != c.want {
			t.Errorf("topLevelObjects(%q) = %d (%v), want %d", c.in, len(got), got, c.want)
		}
	}
}

func TestParseCSV(t *testing.T) {
	cases := map[string][]string{
		"":                nil,
		"net":             {"net"},
		"net, fs,peer":    {"net", "fs", "peer"},
		"  ,  , fs,  ,  ": {"fs"},
	}
	for in, want := range cases {
		got := parseCSV(in)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("parseCSV(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestBuildSystemPrompt_IncludesOnlyAllowedTools(t *testing.T) {
	p := buildSystemPrompt("## my skill\n\nrules here", []string{"net"})
	if !strings.Contains(p, "net_fetch") {
		t.Error("expected net_fetch in prompt when net allowed")
	}
	if strings.Contains(p, "fs_read") {
		t.Error("fs_read must NOT appear when fs not in allow-list")
	}
	if !strings.Contains(p, "my skill") {
		t.Error("skill body must be embedded in system prompt")
	}
	if !strings.Contains(p, `{"answer"`) {
		t.Error("response format contract missing from prompt")
	}
}
