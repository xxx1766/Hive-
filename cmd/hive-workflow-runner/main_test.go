package main

import (
	"strings"
	"testing"
)

func TestParsePlannerReply_DirectJSON(t *testing.T) {
	wf, err := parsePlannerReply(`{"steps":[{"id":"a","tool":"net_fetch","args":{}}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(wf.Steps) != 1 || wf.Steps[0].ID != "a" {
		t.Fatalf("bad parse: %+v", wf)
	}
}

func TestParsePlannerReply_FencedJSON(t *testing.T) {
	in := "Thinking... here's the plan:\n\n```json\n{\"steps\":[{\"id\":\"a\",\"tool\":\"fs_read\",\"args\":{\"path\":\"/data/x\"}}]}\n```"
	wf, err := parsePlannerReply(in)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Steps[0].Tool != "fs_read" {
		t.Fatalf("fenced extraction failed: %+v", wf)
	}
}

func TestParsePlannerReply_GarbageRejected(t *testing.T) {
	if _, err := parsePlannerReply("I cannot comply"); err == nil {
		t.Fatal("expected rejection for prose-only reply")
	}
}

func TestParsePlannerReply_MalformedJSON(t *testing.T) {
	if _, err := parsePlannerReply(`{"steps": not-json}`); err == nil {
		t.Fatal("expected rejection for invalid JSON")
	}
}

func TestParseCSV(t *testing.T) {
	got := parseCSV("net, fs ,  , peer")
	want := []string{"net", "fs", "peer"}
	if len(got) != len(want) {
		t.Fatalf("len: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q want %q", i, got[i], want[i])
		}
	}
}

func TestPlannerInstructions_OnlyAllowedToolsListed(t *testing.T) {
	p := plannerInstructions([]string{"net"})
	if !strings.Contains(p, "net_fetch") {
		t.Error("net_fetch missing")
	}
	if strings.Contains(p, "fs_read") {
		t.Error("fs_read should be gated out")
	}
	if !strings.Contains(p, `"steps"`) {
		t.Error("schema missing")
	}
}
