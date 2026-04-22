package protocol

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
)

func TestMessageClassification(t *testing.T) {
	req, _ := NewRequest(1, "foo", map[string]int{"x": 2})
	if !req.IsRequest() {
		t.Fatalf("request misclassified: %+v", req)
	}
	if req.IsNotification() || req.IsResponse() {
		t.Fatalf("request reports as notification/response")
	}

	notif, _ := NewNotification("evt", nil)
	if !notif.IsNotification() {
		t.Fatalf("notification misclassified")
	}

	resp, _ := NewResponse(req.ID, 42)
	if !resp.IsResponse() {
		t.Fatalf("response misclassified")
	}
}

func TestCodecRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	msgs := []*Message{
		mustReq(t, 1, "a", nil),
		mustReq(t, 2, "b", map[string]any{"k": "v"}),
		mustNotif(t, "log", map[string]any{"msg": "hi"}),
	}
	for _, m := range msgs {
		if err := w.Write(m); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	r := NewReader(&buf)
	for i, want := range msgs {
		got, err := r.Read()
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got.Method != want.Method {
			t.Errorf("msg %d method: got %q want %q", i, got.Method, want.Method)
		}
		if string(got.ID) != string(want.ID) {
			t.Errorf("msg %d id: got %s want %s", i, got.ID, want.ID)
		}
	}

	if _, err := r.Read(); err != ErrClosed {
		t.Fatalf("expected ErrClosed after drain, got %v", err)
	}
}

func TestWriterConcurrentSafe(t *testing.T) {
	// Multiple goroutines writing the same Writer must not interleave bytes.
	var buf bytes.Buffer
	w := NewWriter(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg, _ := NewNotification("log", map[string]any{"i": i})
			_ = w.Write(msg)
		}(i)
	}
	wg.Wait()

	// Every line must parse cleanly.
	r := NewReader(&buf)
	seen := 0
	for {
		m, err := r.Read()
		if err == ErrClosed || err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if m.Method != "log" {
			t.Fatalf("interleaved write corrupted output")
		}
		seen++
	}
	if seen != 50 {
		t.Fatalf("want 50 messages, got %d", seen)
	}
}

func TestErrorEnvelope(t *testing.T) {
	e := NewError(ErrCodeQuotaExceeded, "over budget")
	resp := NewErrorResponse(json.RawMessage("1"), e)
	b, _ := json.Marshal(resp)
	if !strings.Contains(string(b), `"code":-33002`) {
		t.Fatalf("missing code in %s", b)
	}
	if !strings.Contains(string(b), "over budget") {
		t.Fatalf("missing message in %s", b)
	}
}

func mustReq(t *testing.T, id int, method string, params any) *Message {
	t.Helper()
	m, err := NewRequest(id, method, params)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	return m
}

func mustNotif(t *testing.T, method string, params any) *Message {
	t.Helper()
	m, err := NewNotification(method, params)
	if err != nil {
		t.Fatalf("NewNotification: %v", err)
	}
	return m
}
