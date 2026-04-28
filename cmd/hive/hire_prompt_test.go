package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// AllBlank: pressing Enter through every prompt yields zero-value overrides.
// This is the "I just want manifest defaults" path — must not generate a
// non-nil quota envelope or stray volume entries.
func TestPromptHireOverrides_AllBlank(t *testing.T) {
	// 4 prompts (rank, tokens, http) + at least one volume blank line to end the volume loop.
	in := strings.NewReader("\n\n\n\n")
	var out bytes.Buffer
	rank, quota, vols, err := promptHireOverrides(in, &out, "test:0.1.0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rank != "" {
		t.Errorf("rank=%q want empty", rank)
	}
	if quota != nil {
		t.Errorf("quota=%s want nil", string(quota))
	}
	if len(vols) != 0 {
		t.Errorf("vols=%+v want empty", vols)
	}
}

// FullSet: every prompt answered. Verifies parsing of model:N tokens, http
// int, and two volume mounts with different modes.
func TestPromptHireOverrides_FullSet(t *testing.T) {
	input := strings.Join([]string{
		"manager",
		"openai/gpt-5.4-mini:50000",
		"30",
		"paper-corpus:/shared/corpus:ro",
		"paper-draft:/shared/draft:rw",
		"",
	}, "\n") + "\n"
	in := strings.NewReader(input)
	var out bytes.Buffer
	rank, quota, vols, err := promptHireOverrides(in, &out, "writer:0.1.0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rank != "manager" {
		t.Fatalf("rank=%q want manager", rank)
	}
	var qo struct {
		Tokens   map[string]int `json:"tokens"`
		APICalls map[string]int `json:"api_calls"`
	}
	if err := json.Unmarshal(quota, &qo); err != nil {
		t.Fatalf("quota unmarshal: %v", err)
	}
	if qo.Tokens["openai/gpt-5.4-mini"] != 50000 {
		t.Errorf("tokens=%+v", qo.Tokens)
	}
	if qo.APICalls["http"] != 30 {
		t.Errorf("api_calls=%+v", qo.APICalls)
	}
	if len(vols) != 2 {
		t.Fatalf("vols len=%d", len(vols))
	}
	if vols[0].Name != "paper-corpus" || vols[0].Mode != "ro" || vols[0].Mountpoint != "/shared/corpus" {
		t.Errorf("vols[0]=%+v", vols[0])
	}
	if vols[1].Name != "paper-draft" || vols[1].Mode != "rw" || vols[1].Mountpoint != "/shared/draft" {
		t.Errorf("vols[1]=%+v", vols[1])
	}
}

// BadRank: typing a non-existent rank should surface a clear error rather
// than silently passing it to the daemon (where it would fail with a less
// friendly message).
func TestPromptHireOverrides_BadRank(t *testing.T) {
	in := strings.NewReader("guru\n")
	var out bytes.Buffer
	_, _, _, err := promptHireOverrides(in, &out, "x:0.1.0")
	if err == nil {
		t.Fatal("want error for unknown rank")
	}
	if !strings.Contains(err.Error(), "rank") {
		t.Errorf("err=%v should mention 'rank'", err)
	}
}

// BadTokens: non-int budget after the colon must error so the user can
// retype rather than silently dropping the override.
func TestPromptHireOverrides_BadTokens(t *testing.T) {
	in := strings.NewReader("\nopenai/gpt-5.4-mini:notanumber\n")
	var out bytes.Buffer
	_, _, _, err := promptHireOverrides(in, &out, "x:0.1.0")
	if err == nil {
		t.Fatal("want error for non-int token budget")
	}
}

// BadVolume: invalid volume spec is recoverable — the prompt surfaces the
// error and re-asks. Blank line then ends the loop with whatever volumes
// were successfully parsed.
func TestPromptHireOverrides_BadVolumeRecoverable(t *testing.T) {
	input := strings.Join([]string{
		"",                                  // rank blank
		"",                                  // tokens blank
		"",                                  // http blank
		"missing-mountpoint",                // bad volume → loop re-asks
		"good:/m:rw",                        // good
		"",                                  // end loop
	}, "\n") + "\n"
	in := strings.NewReader(input)
	var out bytes.Buffer
	_, _, vols, err := promptHireOverrides(in, &out, "x:0.1.0")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(vols) != 1 || vols[0].Name != "good" {
		t.Errorf("vols=%+v", vols)
	}
	if !strings.Contains(out.String(), "try again") {
		t.Errorf("output should warn the user to retry: %s", out.String())
	}
}

// EOF mid-prompt: Ctrl-D is "use defaults for everything remaining". Volume
// loop should also exit cleanly (the read after EOF returns "" + nil and
// the empty string ends the volume loop).
func TestPromptHireOverrides_EOFMidway(t *testing.T) {
	in := strings.NewReader("staff\n")
	var out bytes.Buffer
	rank, quota, vols, err := promptHireOverrides(in, &out, "x:0.1.0")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if rank != "staff" {
		t.Errorf("rank=%q", rank)
	}
	if quota != nil || len(vols) != 0 {
		t.Errorf("quota=%s vols=%+v", string(quota), vols)
	}
}
