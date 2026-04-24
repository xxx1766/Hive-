// Package agent wraps a single Agent subprocess and exposes it as a
// full-duplex JSON-RPC peer to the rest of the daemon.
//
// Every Agent is a child process. It reads newline-delimited JSON-RPC from
// stdin and writes to stdout. Both sides may initiate requests:
//
//   Hive  → Agent:  task/run, peer/recv, shutdown
//   Agent → Hive:   fs/*, net/fetch, llm/complete, peer/send, task/done, log
//
// Conn demuxes responses to Hive-initiated calls (via pending map) from
// Agent-initiated requests (which are dispatched to registered handlers).
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/anne-x/hive/internal/protocol"
)

// Handler processes a request initiated by the Agent. If err is a
// *protocol.Error it is forwarded verbatim; other errors map to Internal.
type Handler func(ctx context.Context, params json.RawMessage) (result any, err error)

// Conn is the daemon-side endpoint of a running Agent.
type Conn struct {
	// ImageName is how the router and peers address this Agent.
	ImageName string

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	rd      *protocol.Reader
	wr      *protocol.Writer

	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan *protocol.Message

	handlersMu sync.RWMutex
	handlers   map[string]Handler

	ctx    context.Context
	cancel context.CancelFunc

	done    chan struct{}
	exitErr error

	// initErrPipe is the read end of the sandbox init helper's error pipe
	// (ns.NewAgentCommand attaches the write end as child FD 3). nil when
	// sandboxing is disabled. The init helper closes its end either after
	// writing a failure blob or right before syscall.Exec — in both cases
	// readInitErrPipe unblocks and stores the result in initErrData.
	initErrPipe *os.File
	initErrData []byte
	initErrDone chan struct{}
}

// New returns a Conn around cmd. cmd must not be started yet; the caller
// should finish configuring SysProcAttr (e.g. namespace cloneflags) before
// calling Start. initErrPipe, if non-nil, is the read end of the sandbox
// init-error pipe (see ns.NewAgentCommand); Start closes it after draining.
func New(imageName string, cmd *exec.Cmd, initErrPipe *os.File) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		ImageName:   imageName,
		cmd:         cmd,
		pending:     make(map[int64]chan *protocol.Message),
		handlers:    make(map[string]Handler),
		ctx:         ctx,
		cancel:      cancel,
		done:        make(chan struct{}),
		initErrPipe: initErrPipe,
		initErrDone: make(chan struct{}),
	}
	if initErrPipe == nil {
		// No pipe ⇒ nothing to wait for; close immediately so WaitInit
		// and waitLoop don't block.
		close(c.initErrDone)
	}
	return c
}

// Handle registers a handler for an Agent→Hive method.
// Register handlers before Start; concurrent calls during Start are safe
// but handlers added after a matching request arrives may miss it.
func (c *Conn) Handle(method string, h Handler) {
	c.handlersMu.Lock()
	defer c.handlersMu.Unlock()
	c.handlers[method] = h
}

// Start launches the child process and kicks off the read loop.
func (c *Conn) Start() error {
	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	// Stderr goes wherever the caller wired it (usually a log file).
	c.stdin = stdin
	c.stdout = stdout
	c.rd = protocol.NewReader(stdout)
	c.wr = protocol.NewWriter(stdin)

	if err := c.cmd.Start(); err != nil {
		if c.initErrPipe != nil {
			_ = c.initErrPipe.Close()
			close(c.initErrDone)
		}
		return fmt.Errorf("start: %w", err)
	}

	// The child inherited its own copies of every ExtraFiles entry; the
	// parent must close its originals or the pipes' write sides never see
	// EOF (the child is the only legitimate writer).
	for _, f := range c.cmd.ExtraFiles {
		_ = f.Close()
	}

	if c.initErrPipe != nil {
		go c.readInitErrPipe()
	}

	go c.readLoop()
	go c.waitLoop()
	return nil
}

// readInitErrPipe drains the sandbox init helper's error pipe until EOF.
// EOF with zero bytes ⇒ sandbox setup succeeded (helper closed its end
// before syscall.Exec). Non-empty bytes ⇒ setup failed and the helper
// wrote the reason before exit(1).
func (c *Conn) readInitErrPipe() {
	defer close(c.initErrDone)
	const maxBytes = 4096 // plenty for a stack trace; bounded for safety
	data, _ := io.ReadAll(io.LimitReader(c.initErrPipe, maxBytes))
	_ = c.initErrPipe.Close()
	c.mu.Lock()
	c.initErrData = data
	c.mu.Unlock()
}

// WaitInit blocks until the sandbox init helper finishes (either by
// closing FD 3 on success or by writing a diagnostic and exiting). Returns
// a non-nil error iff the helper reported a setup failure — callers should
// treat that as the hire failing even though cmd.Start() reported success.
// Safe to call when sandboxing is disabled (returns nil immediately).
func (c *Conn) WaitInit() error {
	<-c.initErrDone
	c.mu.Lock()
	data := c.initErrData
	c.mu.Unlock()
	if len(data) == 0 {
		return nil
	}
	return errors.New(strings.TrimSpace(string(data)))
}

// Done returns a channel closed when the Agent has exited.
func (c *Conn) Done() <-chan struct{} { return c.done }

// ExitErr reports why the Agent exited (nil if clean, non-nil otherwise).
// Only valid after Done fires.
func (c *Conn) ExitErr() error { return c.exitErr }

// Call sends a Hive→Agent request and waits for the response.
func (c *Conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	msg, err := protocol.NewRequest(id, method, params)
	if err != nil {
		return nil, err
	}

	ch := make(chan *protocol.Message, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.wr.Write(msg); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("agent exited")
	}
}

// Notify sends a Hive→Agent notification (fire-and-forget).
func (c *Conn) Notify(method string, params any) error {
	msg, err := protocol.NewNotification(method, params)
	if err != nil {
		return err
	}
	return c.wr.Write(msg)
}

// Shutdown asks the Agent to terminate gracefully and closes stdin.
// waitLoop will then clean up when the process actually exits.
func (c *Conn) Shutdown(reason string) error {
	// Best-effort notify; ignore errors because the Agent may already be gone.
	_ = c.Notify("shutdown", map[string]string{"reason": reason})
	c.cancel()
	if c.stdin != nil {
		_ = c.stdin.Close()
	}
	return nil
}

// Kill force-terminates the Agent.
func (c *Conn) Kill() error {
	if c.cmd.Process == nil {
		return nil
	}
	return c.cmd.Process.Kill()
}

func (c *Conn) readLoop() {
	for {
		msg, err := c.rd.Read()
		if err != nil {
			return
		}
		switch {
		case msg.IsResponse():
			var id int64
			if err := json.Unmarshal(msg.ID, &id); err != nil {
				continue
			}
			c.mu.Lock()
			ch, ok := c.pending[id]
			c.mu.Unlock()
			if ok {
				ch <- msg
			}
		case msg.IsRequest():
			// Requests may do slow work (fs/net/llm proxy); dispatch async
			// so readLoop keeps draining the Agent's stdout.
			go c.dispatch(msg)
		case msg.IsNotification():
			// Notifications are dispatched synchronously to preserve
			// Agent-emitted ordering (log before task/done matters: the
			// CLI's notify sink detaches as soon as task/done fires).
			c.dispatch(msg)
		}
	}
}

func (c *Conn) dispatch(msg *protocol.Message) {
	c.handlersMu.RLock()
	h, ok := c.handlers[msg.Method]
	c.handlersMu.RUnlock()

	if !ok {
		// Notifications we don't handle are dropped silently.
		if msg.IsRequest() {
			_ = c.wr.Write(protocol.NewErrorResponse(msg.ID, protocol.ErrMethodNotFound(msg.Method)))
		}
		return
	}

	result, err := h(c.ctx, msg.Params)
	if msg.IsNotification() {
		return // no response expected
	}
	if err != nil {
		perr := asProtocolError(err)
		_ = c.wr.Write(protocol.NewErrorResponse(msg.ID, perr))
		return
	}
	resp, rerr := protocol.NewResponse(msg.ID, result)
	if rerr != nil {
		_ = c.wr.Write(protocol.NewErrorResponse(msg.ID, protocol.ErrInternal(rerr.Error())))
		return
	}
	_ = c.wr.Write(resp)
}

func (c *Conn) waitLoop() {
	err := c.cmd.Wait()
	// If the init helper wrote to FD 3 before dying, enrich the exit
	// error with its diagnostic. readInitErrPipe is guaranteed to finish
	// by the time Wait returns (child exit → pipe write side closed → EOF).
	<-c.initErrDone
	c.mu.Lock()
	data := c.initErrData
	c.mu.Unlock()
	if err != nil && len(data) > 0 {
		err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(data)))
	}
	c.exitErr = err
	c.cancel()
	close(c.done)
	// Fail any pending calls.
	c.mu.Lock()
	for _, ch := range c.pending {
		close(ch)
	}
	c.pending = nil
	c.mu.Unlock()
}

func asProtocolError(err error) *protocol.Error {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe
	}
	return protocol.ErrInternal(err.Error())
}
