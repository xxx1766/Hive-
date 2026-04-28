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
	// 5 prompts (rank, model, tokens, http, then a volume blank line).
	in := strings.NewReader("\n\n\n\n\n")
	var out bytes.Buffer
	rank, model, quota, vols, err := promptHireOverrides(in, &out, "test:0.1.0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rank != "" {
		t.Errorf("rank=%q want empty", rank)
	}
	if model != "" {
		t.Errorf("model=%q want empty", model)
	}
	if quota != nil {
		t.Errorf("quota=%s want nil", string(quota))
	}
	if len(vols) != 0 {
		t.Errorf("vols=%+v want empty", vols)
	}
}

// FullSet: every prompt answered. Verifies parsing of model:N tokens, http
// int, and two volume mounts with different modes when model prompt was
// blank (legacy "<model>:<int>" tokens form).
func TestPromptHireOverrides_FullSet_NoModel(t *testing.T) {
	input := strings.Join([]string{
		"manager",
		"",                              // model blank
		"openai/gpt-5.4-mini:50000",     // tokens in <model>:<int> form
		"30",
		"paper-corpus:/shared/corpus:ro",
		"paper-draft:/shared/draft:rw",
		"",
	}, "\n") + "\n"
	in := strings.NewReader(input)
	var out bytes.Buffer
	rank, model, quota, vols, err := promptHireOverrides(in, &out, "writer:0.1.0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if rank != "manager" {
		t.Fatalf("rank=%q want manager", rank)
	}
	if model != "" {
		t.Fatalf("model=%q want empty (user skipped model prompt)", model)
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
}

// ModelAutoFillsTokenKey: when user gives a model, tokens prompt expects a
// plain integer and the quota key is auto-set to that model. This is the
// expected GMI workflow: type model once, get the budget keyed off it
// without retyping the long vendor/model id.
func TestPromptHireOverrides_ModelAutoFillsTokenKey(t *testing.T) {
	input := strings.Join([]string{
		"",                          // rank blank
		"openai/gpt-5.4-mini",       // model set
		"75000",                     // tokens as plain int → key = openai/gpt-5.4-mini
		"",                          // http blank
		"",                          // volume blank-line ends loop
	}, "\n") + "\n"
	in := strings.NewReader(input)
	var out bytes.Buffer
	_, model, quota, _, err := promptHireOverrides(in, &out, "x:0.1.0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if model != "openai/gpt-5.4-mini" {
		t.Fatalf("model=%q", model)
	}
	var qo struct {
		Tokens map[string]int `json:"tokens"`
	}
	if err := json.Unmarshal(quota, &qo); err != nil {
		t.Fatalf("quota unmarshal: %v", err)
	}
	if qo.Tokens["openai/gpt-5.4-mini"] != 75000 {
		t.Errorf("tokens=%+v want {openai/gpt-5.4-mini: 75000}", qo.Tokens)
	}
	// The prompt label should mention the model name so the user sees the
	// auto-fill explicitly rather than wondering what key got used.
	if !strings.Contains(out.String(), "openai/gpt-5.4-mini") {
		t.Errorf("expected token prompt to mention the chosen model; got:\n%s", out.String())
	}
}

// ModelAlone: model set, but tokens left blank. Should not produce a quota
// override at all (no key, no budget — just the env override travels).
func TestPromptHireOverrides_ModelAlone(t *testing.T) {
	input := strings.Join([]string{
		"",                       // rank
		"deepseek-ai/DeepSeek-V4-Pro", // model
		"",                       // tokens blank
		"",                       // http blank
		"",                       // vols
	}, "\n") + "\n"
	in := strings.NewReader(input)
	var out bytes.Buffer
	_, model, quota, _, err := promptHireOverrides(in, &out, "x:0.1.0")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if model != "deepseek-ai/DeepSeek-V4-Pro" {
		t.Errorf("model=%q", model)
	}
	if quota != nil {
		t.Errorf("quota should be nil when only model set, got %s", string(quota))
	}
}

// BadRank: typing a non-existent rank should surface a clear error rather
// than silently passing it to the daemon (where it would fail with a less
// friendly message).
func TestPromptHireOverrides_BadRank(t *testing.T) {
	in := strings.NewReader("guru\n")
	var out bytes.Buffer
	_, _, _, _, err := promptHireOverrides(in, &out, "x:0.1.0")
	if err == nil {
		t.Fatal("want error for unknown rank")
	}
	if !strings.Contains(err.Error(), "rank") {
		t.Errorf("err=%v should mention 'rank'", err)
	}
}

// BadTokens: non-int budget after the colon (model-blank path) must error
// so the user can retype rather than silently dropping the override.
func TestPromptHireOverrides_BadTokens(t *testing.T) {
	in := strings.NewReader("\n\nopenai/gpt-5.4-mini:notanumber\n")
	var out bytes.Buffer
	_, _, _, _, err := promptHireOverrides(in, &out, "x:0.1.0")
	if err == nil {
		t.Fatal("want error for non-int token budget")
	}
}

// BadVolume: invalid volume spec is recoverable — the prompt surfaces the
// error and re-asks. Blank line then ends the loop with whatever volumes
// were successfully parsed.
func TestPromptHireOverrides_BadVolumeRecoverable(t *testing.T) {
	input := strings.Join([]string{
		"",                                  // rank
		"",                                  // model
		"",                                  // tokens
		"",                                  // http
		"missing-mountpoint",                // bad volume → loop re-asks
		"good:/m:rw",                        // good
		"",                                  // end loop
	}, "\n") + "\n"
	in := strings.NewReader(input)
	var out bytes.Buffer
	_, _, _, vols, err := promptHireOverrides(in, &out, "x:0.1.0")
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
	rank, model, quota, vols, err := promptHireOverrides(in, &out, "x:0.1.0")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if rank != "staff" {
		t.Errorf("rank=%q", rank)
	}
	if model != "" || quota != nil || len(vols) != 0 {
		t.Errorf("model=%q quota=%s vols=%+v", model, string(quota), vols)
	}
}
