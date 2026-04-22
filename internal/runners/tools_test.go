package runners

import (
	"strings"
	"testing"
)

func TestToolGroup(t *testing.T) {
	cases := map[string]string{
		"net_fetch":    GroupNet,
		"fs_read":      GroupFS,
		"fs_write":     GroupFS,
		"fs_list":      GroupFS,
		"peer_send":    GroupPeer,
		"llm_complete": GroupLLM,
		"unknown":      "",
	}
	for in, want := range cases {
		if got := ToolGroup(in); got != want {
			t.Errorf("ToolGroup(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToolAllowed(t *testing.T) {
	allow := []string{"net", "fs"}
	if !ToolAllowed("net_fetch", allow) {
		t.Error("net_fetch should be allowed")
	}
	if !ToolAllowed("fs_read", allow) {
		t.Error("fs_read should be allowed")
	}
	if ToolAllowed("peer_send", allow) {
		t.Error("peer_send should NOT be allowed")
	}
	if ToolAllowed("unknown_tool", allow) {
		t.Error("unknown tool should be rejected")
	}
}

func TestResultText_Truncates(t *testing.T) {
	long := strings.Repeat("x", 5000)
	s := ResultText(long, 100)
	if len(s) > 200 {
		// s is JSON-encoded (quotes) so +2, plus the trailing ellipsis rune
		// is multi-byte — 200 is a loose but safe upper bound.
		t.Fatalf("truncation failed: len=%d", len(s))
	}
	if !strings.HasSuffix(s, "…") {
		t.Error("truncation should end with ellipsis")
	}
}

func TestResultText_NoTruncate(t *testing.T) {
	if got := ResultText("short", 0); got != `"short"` {
		t.Errorf("got %q", got)
	}
}

func TestGetInt_Float64(t *testing.T) {
	// JSON numbers unmarshal to float64; verify coercion.
	m := map[string]any{"n": float64(42)}
	if getInt(m, "n") != 42 {
		t.Fatalf("float64 coercion failed: %d", getInt(m, "n"))
	}
}

func TestMessagesFromArgs_StructuredList(t *testing.T) {
	m := map[string]any{
		"messages": []any{
			map[string]any{"role": "system", "content": "you are a ..."},
			map[string]any{"role": "user", "content": "hello"},
		},
	}
	msgs := messagesFromArgs(m)
	if len(msgs) != 2 {
		t.Fatalf("got %d messages", len(msgs))
	}
	if msgs[0].Role != "system" || msgs[1].Content != "hello" {
		t.Errorf("bad parse: %+v", msgs)
	}
}
