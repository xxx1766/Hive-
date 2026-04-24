package agent

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// WaitInit is the narrow contract the sandbox error-pipe hangs on: empty
// EOF ⇒ sandbox setup succeeded; non-empty bytes ⇒ surface them as an
// error. The happy path must not block (so the no-sandbox case is
// instant) and non-empty must round-trip the diagnostic text.

func TestWaitInit_NoPipe(t *testing.T) {
	c := New("t", exec.Command("true"), nil)
	done := make(chan error, 1)
	go func() { done <- c.WaitInit() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitInit nil-pipe: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("WaitInit blocked with nil pipe")
	}
}

func TestWaitInit_EmptyPipeMeansSuccess(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c := New("t", exec.Command("true"), r)
	_ = w.Close() // child equivalent of "closed FD 3 before syscall.Exec"
	go c.readInitErrPipe()
	if err := c.WaitInit(); err != nil {
		t.Fatalf("empty pipe should mean success, got: %v", err)
	}
}

func TestWaitInit_SurfacesDiagnostic(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	c := New("t", exec.Command("true"), r)
	go func() {
		_, _ = w.WriteString("setup: pivot_root: operation not permitted\n")
		_ = w.Close()
	}()
	go c.readInitErrPipe()

	done := make(chan error, 1)
	go func() { done <- c.WaitInit() }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "pivot_root") {
			t.Fatalf("want pivot_root diagnostic, got: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WaitInit did not return within 2s")
	}
}
