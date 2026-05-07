package main

// peer_call / peer_call_many dispatch for the workflow runner.
//
// These mirror cmd/hive-skill-runner's pair byte-for-byte (same
// semantics, same result shapes, same timeout knobs); duplicated here
// because each runner intercepts these tools BEFORE the generic
// runners.DispatchTool — they need awaiter-registry access that the
// generic dispatcher can't provide.
//
// Tested at the awaiter layer in internal/peerawait/peerawait_test.go;
// integration coverage lives in the static-flow demo (a kind: workflow
// agent that peer_calls a kind: skill agent and uses the reply as its
// task output).

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/anne-x/hive/internal/peerawait"
	hive "github.com/anne-x/hive/sdk/go"
)

// peerCallTimeout is the per-call deadline for peer_call. Same default
// as skill-runner — sized for a downstream Agent that needs to read the
// corpus, run an LLM completion, and write a section back.
const peerCallTimeout = 60 * time.Second

// peerCallTimeoutCap bounds args.timeout_seconds; rejects unreasonable
// values that would let one stuck downstream peg the caller's task.
const peerCallTimeoutCap = 5 * time.Minute

// dispatchPeerCall implements the synchronous peer-call tool for a
// workflow step. Args: {"to": <name>, "payload": <any>,
// "timeout_seconds": <int, optional>}. Returns
// {"from": <name>, "payload": <reply>} on success — same shape as
// skill-runner so a downstream step can reference
// $steps.<id>.payload uniformly.
func dispatchPeerCall(ctx context.Context, a *hive.Agent, args map[string]any, convID string, aw *peerawait.Awaiter) (any, error) {
	to, _ := args["to"].(string)
	if to == "" {
		return nil, fmt.Errorf("peer_call: to is required")
	}
	if convID == "" {
		// No conversation context — peer_call needs conv_id to route
		// the reply back. Fail loudly instead of routing to fallback.
		return nil, fmt.Errorf("peer_call: no conv_id in scope; peer_call only works inside a Conversation")
	}
	timeout := peerCallTimeout
	if t, ok := args["timeout_seconds"].(float64); ok && t > 0 {
		d := time.Duration(t) * time.Second
		if d > peerCallTimeoutCap {
			d = peerCallTimeoutCap
		}
		timeout = d
	}

	ch, cancel := aw.Register(to, convID)
	defer cancel()

	if err := a.PeerSend(ctx, to, args["payload"], hive.WithConv(convID)); err != nil {
		return nil, fmt.Errorf("peer_call: send failed: %w", err)
	}

	select {
	case reply, ok := <-ch:
		if !ok {
			return nil, fmt.Errorf("peer_call: awaiter cancelled before reply")
		}
		return map[string]any{
			"from":    reply.From,
			"payload": reply.Payload,
		}, nil
	case <-time.After(timeout):
		return nil, fmt.Errorf("peer_call: timeout after %s waiting for reply from %s", timeout, to)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// dispatchPeerCallMany implements parallel fan-out — peer_call to N
// peers concurrently. Args:
//
//	{"calls": [{"to": <name>, "payload": <any>}, ...],
//	 "timeout_seconds": <int, optional>}
//
// Returns {"replies": [{"to","ok","from","payload"|"error"}]} in the
// original calls order so a downstream step can reference
// $steps.<id>.replies and walk by index without re-keying.
func dispatchPeerCallMany(ctx context.Context, a *hive.Agent, args map[string]any, convID string, aw *peerawait.Awaiter) (any, error) {
	if convID == "" {
		return nil, fmt.Errorf("peer_call_many: no conv_id in scope")
	}
	rawCalls, _ := args["calls"].([]any)
	if len(rawCalls) == 0 {
		return nil, fmt.Errorf("peer_call_many: calls is required and must be a non-empty list")
	}
	timeout := peerCallTimeout
	if t, ok := args["timeout_seconds"].(float64); ok && t > 0 {
		d := time.Duration(t) * time.Second
		if d > peerCallTimeoutCap {
			d = peerCallTimeoutCap
		}
		timeout = d
	}

	type call struct {
		to      string
		payload any
	}
	calls := make([]call, 0, len(rawCalls))
	for i, rc := range rawCalls {
		obj, ok := rc.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("peer_call_many: calls[%d] not an object", i)
		}
		to, _ := obj["to"].(string)
		if to == "" {
			return nil, fmt.Errorf("peer_call_many: calls[%d].to is required", i)
		}
		calls = append(calls, call{to: to, payload: obj["payload"]})
	}

	// Register all awaiters synchronously BEFORE any send — guarantees
	// no reply can race past its own awaiter.
	chans := make([]<-chan *hive.PeerMessage, len(calls))
	cancels := make([]func(), len(calls))
	for i, c := range calls {
		chans[i], cancels[i] = aw.Register(c.to, convID)
	}
	defer func() {
		for _, c := range cancels {
			c()
		}
	}()

	type result struct {
		To      string `json:"to"`
		OK      bool   `json:"ok"`
		From    string `json:"from,omitempty"`
		Payload any    `json:"payload,omitempty"`
		Error   string `json:"error,omitempty"`
	}
	results := make([]result, len(calls))
	var wg sync.WaitGroup
	deadline := time.After(timeout)
	for i := range calls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i].To = calls[i].to
			if err := a.PeerSend(ctx, calls[i].to, calls[i].payload, hive.WithConv(convID)); err != nil {
				results[i].Error = "send failed: " + err.Error()
				return
			}
			select {
			case reply, ok := <-chans[i]:
				if !ok || reply == nil {
					results[i].Error = "awaiter cancelled before reply"
					return
				}
				results[i].OK = true
				results[i].From = reply.From
				var p any
				if err := json.Unmarshal(reply.Payload, &p); err == nil {
					results[i].Payload = p
				} else {
					results[i].Payload = string(reply.Payload)
				}
			case <-deadline:
				results[i].Error = fmt.Sprintf("timeout after %s waiting for reply from %s", timeout, calls[i].to)
			case <-ctx.Done():
				results[i].Error = ctx.Err().Error()
			}
		}(i)
	}
	wg.Wait()
	return map[string]any{"replies": results}, nil
}
