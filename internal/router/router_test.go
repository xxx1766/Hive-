package router

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/anne-x/hive/internal/agent"
	"github.com/anne-x/hive/internal/protocol"
)

// The Router talks to *agent.Conn, which wants a started child process. For
// unit tests we construct Conns around an in-process exec.Cmd pointing at a
// tiny echo-style Go program — too heavy. Instead we add a simple
// notification interceptor by giving agent.Conn a way to override its send
// path, but that couples the packages. Easiest pragmatic choice: hit Router
// through its public Send path and assert via the AuthFn callback + error
// codes. Full delivery path is covered by the end-to-end demo.

func TestRouterAuthRejects(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	blocked := errors.New("blocked")
	r := New("room-x", func(from, to string) error {
		if from == "A" && to == "B" {
			return &protocol.Error{Code: protocol.ErrCodePermissionDenied, Message: blocked.Error()}
		}
		return nil
	}, 0)
	go r.Run(ctx)

	// No agents registered — we still exercise the auth gate, which runs
	// before target lookup.
	err := r.Send(ctx, "A", "B", "", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected auth error")
	}
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrCodePermissionDenied {
		t.Fatalf("want permission_denied, got %v", err)
	}
}

func TestRouterTargetMissing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := New("room-x", nil, 0)
	go r.Run(ctx)

	err := r.Send(ctx, "A", "B", "", json.RawMessage(`{}`))
	if err == nil {
		t.Fatal("expected peer-not-found")
	}
	var perr *protocol.Error
	if !errors.As(err, &perr) || perr.Code != protocol.ErrCodePeerNotFound {
		t.Fatalf("want peer_not_found, got %v", err)
	}
}

func TestRouterStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := New("room-x", nil, 1)
	go r.Run(ctx)
	cancel()
	time.Sleep(10 * time.Millisecond)

	// After Run exits, Send must fail promptly rather than block forever.
	done := make(chan struct{})
	go func() {
		_ = r.Send(context.Background(), "A", "B", "", json.RawMessage(`{}`))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Send blocked after router stopped")
	}
}

// Compile-time assurance: Router.Register accepts *agent.Conn so the real
// daemon integration works even without exercising the full path here.
var _ = (*Router).Register
var _ = (*agent.Conn)(nil)
