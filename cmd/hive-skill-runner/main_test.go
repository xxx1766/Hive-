package main

import (
	"reflect"
	"strings"
	"testing"
)

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

func TestToolGroup(t *testing.T) {
	cases := map[string]string{
		"net_fetch":    "net",
		"fs_read":      "fs",
		"fs_write":     "fs",
		"fs_list":      "fs",
		"peer_send":    "peer",
		"llm_complete": "llm",
		"bogus":        "",
	}
	for tool, want := range cases {
		if got := toolGroup(tool); got != want {
			t.Errorf("toolGroup(%q) = %q, want %q", tool, got, want)
		}
	}
}

func TestToolAllowed(t *testing.T) {
	allow := []string{"net", "peer"}
	if !toolAllowed("net_fetch", allow) {
		t.Error("net_fetch should be allowed")
	}
	if toolAllowed("fs_read", allow) {
		t.Error("fs_read should NOT be allowed (fs not in list)")
	}
	if toolAllowed("unknown_tool", allow) {
		t.Error("unknown tool should be rejected")
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

func TestTruncate(t *testing.T) {
	if got := truncate("hello", 10); got != "hello" {
		t.Errorf("short-circuit failed: %q", got)
	}
	if got := truncate("hello world", 5); got != "hello…" {
		t.Errorf("truncate+ellipsis failed: %q", got)
	}
}
