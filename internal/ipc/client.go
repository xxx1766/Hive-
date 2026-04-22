package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/anne-x/hive/internal/protocol"
)

// NotifyReceiver is invoked for every server-pushed notification.
// The client dispatches all notifications to the single handler set via
// SetNotifyHandler. Demo CLI makes one call at a time, so attribution
// is straightforward; if multiplexing is needed later, embed a call ID
// in the notification payload.
type NotifyReceiver func(method string, params json.RawMessage)

// Client is a JSON-RPC 2.0 client over Unix socket with NDJSON framing.
type Client struct {
	conn    net.Conn
	wr      *protocol.Writer
	rd      *protocol.Reader
	nextID  atomic.Int64
	mu      sync.Mutex
	pending map[int64]chan *protocol.Message
	notify  atomic.Value // NotifyReceiver
	closed  chan struct{}
	closeMu sync.Once
}

// Dial opens a connection to the daemon's Unix socket.
func Dial(ctx context.Context, socketPath string) (*Client, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", socketPath, err)
	}
	c := &Client{
		conn:    conn,
		wr:      protocol.NewWriter(conn),
		rd:      protocol.NewReader(conn),
		pending: make(map[int64]chan *protocol.Message),
		closed:  make(chan struct{}),
	}
	c.notify.Store(NotifyReceiver(func(string, json.RawMessage) {}))
	go c.readLoop()
	return c, nil
}

// SetNotifyHandler installs a receiver for server-pushed notifications.
// The default (after Dial) is a no-op sink.
func (c *Client) SetNotifyHandler(fn NotifyReceiver) {
	if fn == nil {
		fn = func(string, json.RawMessage) {}
	}
	c.notify.Store(fn)
}

// Call sends a request and blocks until a response arrives or ctx is done.
// Intermediate notifications are delivered to the registered NotifyReceiver.
func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
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
	case <-c.closed:
		return nil, errors.New("connection closed")
	}
}

// Close terminates the connection.
func (c *Client) Close() error {
	c.closeMu.Do(func() { close(c.closed) })
	return c.conn.Close()
}

func (c *Client) readLoop() {
	defer c.closeMu.Do(func() { close(c.closed) })
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
		case msg.IsNotification():
			if fn, ok := c.notify.Load().(NotifyReceiver); ok && fn != nil {
				fn(msg.Method, msg.Params)
			}
		}
	}
}
