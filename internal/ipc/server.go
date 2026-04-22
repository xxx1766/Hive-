package ipc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/anne-x/hive/internal/protocol"
)

// NotifyFunc lets a Handler push unsolicited notifications to the connected
// client during the lifetime of its call. Used by `room/run` to stream logs.
type NotifyFunc func(method string, params any)

// Handler processes one RPC call. If err is non-nil, it is sent as the
// response's error field. err should be *protocol.Error for well-formed
// error codes; any other error type is wrapped as Internal.
type Handler func(ctx context.Context, params json.RawMessage, notify NotifyFunc) (result any, err error)

// Server is a JSON-RPC 2.0 over Unix-socket dispatcher.
type Server struct {
	socketPath string
	listener   net.Listener

	mu       sync.RWMutex
	handlers map[string]Handler
}

func NewServer(socketPath string) *Server {
	return &Server{
		socketPath: socketPath,
		handlers:   make(map[string]Handler),
	}
}

// Handle registers a method handler. Safe to call before Serve only.
func (s *Server) Handle(method string, h Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}

// Listen creates and binds the Unix socket. Must be called before Serve.
func (s *Server) Listen() error {
	if err := os.MkdirAll(filepath.Dir(s.socketPath), 0o750); err != nil {
		return fmt.Errorf("mkdir socket dir: %w", err)
	}
	// Remove stale socket from a previous crashed daemon.
	_ = os.Remove(s.socketPath)
	l, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.socketPath, err)
	}
	s.listener = l
	return nil
}

// Serve accepts connections until ctx is cancelled or Listener closes.
func (s *Server) Serve(ctx context.Context) error {
	go func() {
		<-ctx.Done()
		_ = s.listener.Close()
	}()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handleConn(ctx, conn)
	}
}

// Close shuts down the listener and removes the socket file.
func (s *Server) Close() error {
	if s.listener != nil {
		_ = s.listener.Close()
	}
	return os.Remove(s.socketPath)
}

func (s *Server) handleConn(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	rd := protocol.NewReader(conn)
	wr := protocol.NewWriter(conn)

	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	for {
		msg, err := rd.Read()
		if err != nil {
			return
		}
		if !msg.IsRequest() {
			// CLI should never send notifications; ignore.
			continue
		}
		// Each call runs in its own goroutine so slow calls don't
		// block others on the same connection.
		go s.dispatch(connCtx, wr, msg)
	}
}

func (s *Server) dispatch(ctx context.Context, wr *protocol.Writer, msg *protocol.Message) {
	s.mu.RLock()
	h, ok := s.handlers[msg.Method]
	s.mu.RUnlock()
	if !ok {
		_ = wr.Write(protocol.NewErrorResponse(msg.ID, protocol.ErrMethodNotFound(msg.Method)))
		return
	}

	notify := func(method string, params any) {
		n, err := protocol.NewNotification(method, params)
		if err != nil {
			return
		}
		_ = wr.Write(n)
	}

	result, err := h(ctx, msg.Params, notify)
	if err != nil {
		perr := asProtocolError(err)
		_ = wr.Write(protocol.NewErrorResponse(msg.ID, perr))
		return
	}
	resp, rerr := protocol.NewResponse(msg.ID, result)
	if rerr != nil {
		_ = wr.Write(protocol.NewErrorResponse(msg.ID, protocol.ErrInternal(rerr.Error())))
		return
	}
	_ = wr.Write(resp)
}

func asProtocolError(err error) *protocol.Error {
	var pe *protocol.Error
	if errors.As(err, &pe) {
		return pe
	}
	return protocol.ErrInternal(err.Error())
}
