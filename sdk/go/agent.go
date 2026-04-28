// Package hive is the Go SDK for writing Hive Agents.
//
// Usage (copy into your Agent's main.go):
//
//	import hive "github.com/anne-x/hive/sdk/go"
//
//	func main() {
//	    a := hive.MustConnect()
//	    defer a.Close()
//	    for task := range a.Tasks() {
//	        task.Reply(map[string]any{"echoed": task.Input})
//	    }
//	}
//
// Under the hood this wraps Hive's JSON-RPC-over-stdio wire protocol with
// channel-based ergonomics: tasks and peer messages arrive on channels,
// so an Agent that wants concurrency just spawns goroutines that read
// from those channels, and Go's select/ctx stays idiomatic throughout.
package hive

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
)

// Task is a work unit delivered by Hive. ConvID is non-empty when the
// task was dispatched as the entry-point of a Conversation; the runner
// should propagate it on outbound PeerSend(... WithConv(t.ConvID)) so
// inter-Agent hops register on the right transcript.
type Task struct {
	ID     string
	Input  json.RawMessage
	ConvID string

	agent *Agent
}

// Reply sends task/done for this task. Call exactly once per Task.
func (t *Task) Reply(output any) error {
	raw, err := json.Marshal(output)
	if err != nil {
		return err
	}
	return t.agent.notify("task/done", map[string]any{
		"task_id": t.ID,
		"output":  json.RawMessage(raw),
	})
}

// Fail sends task/error. Call exactly once per Task.
func (t *Task) Fail(code int, msg string) error {
	return t.agent.notify("task/error", map[string]any{
		"task_id": t.ID,
		"code":    code,
		"message": msg,
	})
}

// PeerMessage is an inbound message from another Agent in the same Room.
type PeerMessage struct {
	From    string
	Payload json.RawMessage
	// ConvID, when non-empty, marks this message as a hop in a Conversation.
	// Reply via PeerSend with WithConv(ConvID) so the round counter advances
	// on the right transcript and the message lands in the persisted log.
	ConvID string
}

// SendOpt customises an outbound peer/send. Today only WithConv exists;
// keeping this as a variadic functional-options pattern means future
// flags (priority, deadline, etc.) won't break callers.
type SendOpt func(*sendOpts)

type sendOpts struct {
	convID string
}

// WithConv tags an outbound peer/send as a Conversation hop. The daemon
// will increment the round counter on the matching transcript and reject
// the call if the cap was already hit. Empty convID is a no-op.
func WithConv(convID string) SendOpt {
	return func(o *sendOpts) { o.convID = convID }
}

// Event is an inbound pub/sub event delivered via events/recv. FromRoom
// is non-empty for cross-Room events (Volume scope) — it's redundant for
// scope="" since FromRoom always equals this Agent's own RoomID there.
type Event struct {
	Scope     string
	FromRoom  string
	FromAgent string
	Seq       uint64
	Payload   json.RawMessage
}

// Subscription is the handle returned by Agent.EventsSubscribe. Range
// over Events() until the channel is closed (Close, or Agent shutdown).
type Subscription struct {
	subID  string
	scope  string
	ch     chan *Event
	agent  *Agent
	closed chan struct{}
	once   sync.Once
}

// Events yields incoming events. Closed when the subscription is cancelled.
func (s *Subscription) Events() <-chan *Event { return s.ch }

// SubID is the daemon-issued opaque token. Mainly useful for logging.
func (s *Subscription) SubID() string { return s.subID }

// Scope returns the wire scope this subscription was opened on.
func (s *Subscription) Scope() string { return s.scope }

// Close cancels the subscription daemon-side and closes the Events channel.
// Idempotent. Safe to call after the Agent has been closed (best-effort
// unsubscribe; the daemon already cleaned up via OnAgentExit).
func (s *Subscription) Close() error {
	var sendErr error
	s.once.Do(func() {
		s.agent.subsMu.Lock()
		delete(s.agent.subs, s.subID)
		s.agent.subsMu.Unlock()
		close(s.closed)
		close(s.ch)
		// Best-effort RPC; if the agent is already closed we swallow.
		sendErr = s.agent.notify("events/unsubscribe", map[string]any{"sub_id": s.subID})
	})
	return sendErr
}

// Agent is the Hive-side handle. Construct via Connect / MustConnect.
type Agent struct {
	rd   *bufio.Reader
	wr   *bufio.Writer
	wrMu sync.Mutex

	nextID atomic.Int64

	pendMu sync.Mutex
	pend   map[int64]chan *wireMessage

	tasks chan *Task
	peers chan *PeerMessage

	subsMu sync.Mutex
	subs   map[string]*Subscription // sub_id → subscription

	closed    chan struct{}
	closeOnce sync.Once
}

type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *wireErr        `json:"error,omitempty"`
}

type wireErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *wireErr) Error() string { return fmt.Sprintf("hive: %s (code=%d)", e.Message, e.Code) }

// Connect attaches to Hive via stdio. Returns an error only if the stdio
// handles are unavailable (pathological).
func Connect() (*Agent, error) {
	a := &Agent{
		rd:     bufio.NewReaderSize(os.Stdin, 64*1024),
		wr:     bufio.NewWriter(os.Stdout),
		pend:   make(map[int64]chan *wireMessage),
		tasks:  make(chan *Task, 16),
		peers:  make(chan *PeerMessage, 16),
		subs:   make(map[string]*Subscription),
		closed: make(chan struct{}),
	}
	go a.readLoop()
	return a, nil
}

// MustConnect is Connect + panic on error. Agents almost always want this.
func MustConnect() *Agent {
	a, err := Connect()
	if err != nil {
		panic(err)
	}
	return a
}

// Close terminates the Agent loop. Safe to call multiple times.
func (a *Agent) Close() error {
	a.closeOnce.Do(func() { close(a.closed) })
	return nil
}

// Done is closed when Hive has signalled shutdown or stdin has EOFed.
func (a *Agent) Done() <-chan struct{} { return a.closed }

// Tasks yields incoming task/run dispatches. Range over it until it's closed.
func (a *Agent) Tasks() <-chan *Task { return a.tasks }

// Peers yields inbound peer messages (peer/recv).
func (a *Agent) Peers() <-chan *PeerMessage { return a.peers }

// Log emits a structured log entry; fields is optional.
func (a *Agent) Log(level, msg string, fields ...map[string]any) {
	p := map[string]any{"level": level, "msg": msg}
	if len(fields) > 0 {
		p["fields"] = fields[0]
	}
	_ = a.notify("log", p)
}

// ── Agent → Hive calls ────────────────────────────────────────────────────

// NetFetch performs an HTTP request through Hive's shared connection pool.
// Returns status, response body, error.
func (a *Agent) NetFetch(ctx context.Context, method, url string, headers map[string]string, body []byte) (int, []byte, error) {
	var res struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers,omitempty"`
		Body    []byte            `json:"body,omitempty"`
	}
	if err := a.call(ctx, "net/fetch", map[string]any{
		"method":  method,
		"url":     url,
		"headers": headers,
		"body":    body,
	}, &res); err != nil {
		return 0, nil, err
	}
	return res.Status, res.Body, nil
}

// LLMComplete calls the daemon's LLM provider. model+messages map to an
// OpenAI-style chat completion (which Hive also accepts for other providers).
func (a *Agent) LLMComplete(ctx context.Context, provider, model string, messages []LLMMessage, maxTokens int) (text string, usage LLMUsage, err error) {
	var res struct {
		Text  string   `json:"text"`
		Usage LLMUsage `json:"usage"`
	}
	if err := a.call(ctx, "llm/complete", map[string]any{
		"provider":   provider,
		"model":      model,
		"messages":   messages,
		"max_tokens": maxTokens,
	}, &res); err != nil {
		return "", LLMUsage{}, err
	}
	return res.Text, res.Usage, nil
}

// LLMMessage mirrors OpenAI's chat message shape.
type LLMMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// LLMUsage is the token accounting returned by providers.
type LLMUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// FSEntry is one entry in fs/list results.
type FSEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// FSRead reads a file from the Agent's sandboxed view.
func (a *Agent) FSRead(ctx context.Context, path string) ([]byte, error) {
	var res struct {
		Data []byte `json:"data"`
	}
	if err := a.call(ctx, "fs/read", map[string]any{"path": path}, &res); err != nil {
		return nil, err
	}
	return res.Data, nil
}

// FSWrite writes (creates or overwrites) a file.
func (a *Agent) FSWrite(ctx context.Context, path string, data []byte) error {
	return a.call(ctx, "fs/write", map[string]any{"path": path, "data": data}, nil)
}

// FSList lists a directory.
func (a *Agent) FSList(ctx context.Context, path string) ([]FSEntry, error) {
	var res struct {
		Entries []FSEntry `json:"entries"`
	}
	if err := a.call(ctx, "fs/list", map[string]any{"path": path}, &res); err != nil {
		return nil, err
	}
	return res.Entries, nil
}

// HireJuniorOpts shapes the optional fields of HireJunior. Only Quota
// is commonly set — daemon will reject the call if the parent's
// remaining budget is below what's requested.
type HireJuniorOpts struct {
	// Tag is a UI label; defaults to the image name when empty.
	Tag string
	// Model overrides the manifest's default LLM model (HIVE_MODEL env).
	Model string
	// Quota tokens / api_calls carved out of the caller's remaining
	// budget. Each key drains the parent's same-named bucket.
	Quota *Quota
	// Volumes the child should mount. Names must already exist in the
	// daemon's volume manager.
	Volumes []VolumeMount
}

// Quota mirrors the daemon's quota wire form. Tokens map keys are model
// names ("gpt-4o-mini", "openai/gpt-5.4-mini"); APICalls keys are
// endpoint categories ("http", "ai_tool:claude-code").
type Quota struct {
	Tokens   map[string]int
	APICalls map[string]int
}

// VolumeMount declares one volume the new subordinate should see.
type VolumeMount struct {
	Name       string
	Mode       string // "ro" or "rw"; defaults to "ro" daemon-side
	Mountpoint string
}

// HireJunior spawns a subordinate Agent in the caller's Room and
// returns its image name (so the caller can PeerSend / WithConv to it).
// Manager+ rank only: the daemon validates rank.CanHire(self, requested)
// and carves opts.Quota out of the parent's remaining budget atomically.
//
// Common usage in a coordinator pattern:
//
//	child, err := a.HireJunior(ctx, "paper-outline:0.1.0", "intern",
//	    hive.HireJuniorOpts{Quota: &hive.Quota{Tokens: map[string]int{"gpt-4o-mini": 5000}}})
//	if err != nil { ... }
//	a.PeerSend(ctx, child, payload, hive.WithConv(convID))
func (a *Agent) HireJunior(ctx context.Context, ref, rank string, opts ...HireJuniorOpts) (string, error) {
	o := HireJuniorOpts{}
	if len(opts) > 0 {
		o = opts[0]
	}
	args := map[string]any{"ref": ref, "rank": rank}
	if o.Tag != "" {
		args["tag"] = o.Tag
	}
	if o.Model != "" {
		args["model"] = o.Model
	}
	if o.Quota != nil {
		args["quota"] = map[string]any{
			"tokens":    o.Quota.Tokens,
			"api_calls": o.Quota.APICalls,
		}
	}
	if len(o.Volumes) > 0 {
		vols := make([]map[string]any, len(o.Volumes))
		for i, v := range o.Volumes {
			vols[i] = map[string]any{"name": v.Name, "mode": v.Mode, "mountpoint": v.Mountpoint}
		}
		args["volumes"] = vols
	}
	var res struct {
		ImageName string `json:"image_name"`
		Rank      string `json:"rank"`
		Parent    string `json:"parent"`
	}
	if err := a.call(ctx, "hire/junior", args, &res); err != nil {
		return "", err
	}
	return res.ImageName, nil
}

// PeerSend delivers a message to another Agent in the same Room. Pass
// WithConv(convID) to make this hop count toward a Conversation; without
// it the message is ad-hoc (no transcript, no round budget).
func (a *Agent) PeerSend(ctx context.Context, to string, payload any, opts ...SendOpt) error {
	o := &sendOpts{}
	for _, fn := range opts {
		fn(o)
	}
	args := map[string]any{"to": to, "payload": payload}
	if o.convID != "" {
		args["conv_id"] = o.convID
	}
	return a.call(ctx, "peer/send", args, nil)
}

// ── Memory API (persistent KV, Room-private or Volume-shared) ────────────

// MemoryPut writes value under key in the given scope.
// scope="" ⇒ Room-private; scope="<volume>" ⇒ cross-Room via named Volume.
func (a *Agent) MemoryPut(ctx context.Context, scope, key string, value []byte) error {
	return a.call(ctx, "memory/put", map[string]any{
		"scope": scope, "key": key, "value": value,
	}, nil)
}

// MemoryGet returns (value, exists, err). A missing key returns
// (nil, false, nil) — not an error — so callers can use it as presence-check.
func (a *Agent) MemoryGet(ctx context.Context, scope, key string) ([]byte, bool, error) {
	var res struct {
		Value  []byte `json:"value,omitempty"`
		Exists bool   `json:"exists"`
	}
	if err := a.call(ctx, "memory/get", map[string]any{
		"scope": scope, "key": key,
	}, &res); err != nil {
		return nil, false, err
	}
	return res.Value, res.Exists, nil
}

// MemoryList returns all keys in the scope whose string starts with prefix
// (use "" for all keys). Result order is lexicographic.
func (a *Agent) MemoryList(ctx context.Context, scope, prefix string) ([]string, error) {
	var res struct {
		Keys []string `json:"keys"`
	}
	if err := a.call(ctx, "memory/list", map[string]any{
		"scope": scope, "prefix": prefix,
	}, &res); err != nil {
		return nil, err
	}
	return res.Keys, nil
}

// MemoryDelete removes key from scope. Missing keys are not an error.
func (a *Agent) MemoryDelete(ctx context.Context, scope, key string) error {
	return a.call(ctx, "memory/delete", map[string]any{
		"scope": scope, "key": key,
	}, nil)
}

// ── Events API (real-time pub/sub, Room-private or Volume-shared) ────────

// EventsPublish broadcasts payload to every Agent currently subscribed on
// the same scope. scope="" reaches subscribers in this Agent's Room only;
// scope="<volume>" reaches subscribers in any Room that hold the same
// Volume name. The publisher itself is excluded — events never echo back.
func (a *Agent) EventsPublish(ctx context.Context, scope string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return a.call(ctx, "events/publish", map[string]any{
		"scope":   scope,
		"payload": json.RawMessage(raw),
	}, nil)
}

// EventsSubscribe opens a subscription on scope and returns a Subscription
// whose Events() channel yields each delivered Event. Cancel by calling
// Close on the returned Subscription, or just drop it and let the Agent
// shutdown clean up daemon-side via OnAgentExit. The returned channel is
// buffered (16); slow consumers will block delivery for this Agent.
func (a *Agent) EventsSubscribe(ctx context.Context, scope string) (*Subscription, error) {
	var res struct {
		SubID string `json:"sub_id"`
	}
	if err := a.call(ctx, "events/subscribe", map[string]any{"scope": scope}, &res); err != nil {
		return nil, err
	}
	if res.SubID == "" {
		return nil, errors.New("hive: empty sub_id from daemon")
	}
	sub := &Subscription{
		subID:  res.SubID,
		scope:  scope,
		ch:     make(chan *Event, 16),
		agent:  a,
		closed: make(chan struct{}),
	}
	a.subsMu.Lock()
	a.subs[res.SubID] = sub
	a.subsMu.Unlock()
	return sub, nil
}

// ── AI tool invocation (Claude Code CLI et al.) ──────────────────────────

// AIToolInvoke runs a CLI-shaped AI tool (MVP: "claude-code") with the
// calling Room's /workspace as cwd. Files Agents write under /workspace
// before this call are visible to the tool; files the tool writes land
// back under /workspace where the Agent can fs_read them.
func (a *Agent) AIToolInvoke(ctx context.Context, tool, prompt string) (string, error) {
	var res struct {
		Output   string `json:"output"`
		Stderr   string `json:"stderr"`
		ExitCode int    `json:"exit_code"`
	}
	if err := a.call(ctx, "ai_tool/invoke", map[string]any{
		"tool": tool, "prompt": prompt,
	}, &res); err != nil {
		return "", err
	}
	return res.Output, nil
}

// ── internals ─────────────────────────────────────────────────────────────

func (a *Agent) readLoop() {
	defer func() {
		_ = a.Close()
		close(a.tasks)
		close(a.peers)
		// Close every live subscription so range-over-Events() exits.
		a.subsMu.Lock()
		for id, s := range a.subs {
			s.once.Do(func() {
				close(s.closed)
				close(s.ch)
			})
			delete(a.subs, id)
		}
		a.subsMu.Unlock()
	}()
	for {
		line, err := a.rd.ReadBytes('\n')
		if len(line) == 0 && err != nil {
			return
		}
		var m wireMessage
		if err := json.Unmarshal(line, &m); err != nil {
			continue
		}
		switch {
		case len(m.ID) > 0 && m.Method == "":
			// Response to a call we initiated.
			var id int64
			if json.Unmarshal(m.ID, &id) != nil {
				continue
			}
			a.pendMu.Lock()
			ch, ok := a.pend[id]
			a.pendMu.Unlock()
			if ok {
				ch <- &m
			}

		case m.Method == "task/run":
			// Ack the request first so Hive's Call unblocks.
			_ = a.respond(m.ID, struct{}{})
			var p struct {
				TaskID string          `json:"task_id"`
				Input  json.RawMessage `json:"input,omitempty"`
				ConvID string          `json:"conv_id,omitempty"`
			}
			_ = json.Unmarshal(m.Params, &p)
			select {
			case a.tasks <- &Task{ID: p.TaskID, Input: p.Input, ConvID: p.ConvID, agent: a}:
			case <-a.closed:
				return
			}

		case m.Method == "peer/recv":
			var p struct {
				From    string          `json:"from"`
				Payload json.RawMessage `json:"payload"`
				ConvID  string          `json:"conv_id"`
			}
			_ = json.Unmarshal(m.Params, &p)
			select {
			case a.peers <- &PeerMessage{From: p.From, Payload: p.Payload, ConvID: p.ConvID}:
			case <-a.closed:
				return
			}

		case m.Method == "events/recv":
			var p struct {
				Scope     string          `json:"scope"`
				SubID     string          `json:"sub_id"`
				FromRoom  string          `json:"from_room"`
				FromAgent string          `json:"from_agent"`
				Seq       uint64          `json:"seq"`
				Payload   json.RawMessage `json:"payload"`
			}
			_ = json.Unmarshal(m.Params, &p)
			a.subsMu.Lock()
			sub, ok := a.subs[p.SubID]
			a.subsMu.Unlock()
			if !ok {
				// Late delivery for a sub we already cancelled — drop.
				continue
			}
			ev := &Event{
				Scope: p.Scope, FromRoom: p.FromRoom, FromAgent: p.FromAgent,
				Seq: p.Seq, Payload: p.Payload,
			}
			select {
			case sub.ch <- ev:
			case <-sub.closed:
				// Subscription cancelled mid-delivery; drop.
			case <-a.closed:
				return
			}

		case m.Method == "shutdown":
			return

		default:
			if len(m.ID) > 0 {
				_ = a.respondError(m.ID, -32601, "method not found: "+m.Method)
			}
		}
	}
}

func (a *Agent) call(ctx context.Context, method string, params any, out any) error {
	id := a.nextID.Add(1)
	idRaw, _ := json.Marshal(id)
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		return err
	}

	reply := make(chan *wireMessage, 1)
	a.pendMu.Lock()
	a.pend[id] = reply
	a.pendMu.Unlock()
	defer func() {
		a.pendMu.Lock()
		delete(a.pend, id)
		a.pendMu.Unlock()
	}()

	if err := a.sendRaw(&wireMessage{
		JSONRPC: "2.0",
		ID:      idRaw,
		Method:  method,
		Params:  paramsRaw,
	}); err != nil {
		return err
	}

	select {
	case resp := <-reply:
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-a.closed:
		return errors.New("agent closed")
	}
}

func (a *Agent) notify(method string, params any) error {
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return a.sendRaw(&wireMessage{
		JSONRPC: "2.0",
		Method:  method,
		Params:  paramsRaw,
	})
}

func (a *Agent) respond(id json.RawMessage, result any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return a.sendRaw(&wireMessage{JSONRPC: "2.0", ID: id, Result: raw})
}

func (a *Agent) respondError(id json.RawMessage, code int, msg string) error {
	return a.sendRaw(&wireMessage{JSONRPC: "2.0", ID: id, Error: &wireErr{Code: code, Message: msg}})
}

func (a *Agent) sendRaw(m *wireMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	a.wrMu.Lock()
	defer a.wrMu.Unlock()
	if _, err := a.wr.Write(append(b, '\n')); err != nil {
		return err
	}
	return a.wr.Flush()
}
