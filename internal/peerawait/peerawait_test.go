package peerawait

import (
	"sync"
	"testing"
	"time"

	hive "github.com/anne-x/hive/sdk/go"
)

func TestAwaiter_DispatchToRegistered(t *testing.T) {
	a := New()
	ch, cancel := a.Register("worker", "C1")
	defer cancel()

	go a.Dispatch(&hive.PeerMessage{From: "worker", ConvID: "C1", Payload: []byte(`"ok"`)})

	select {
	case got := <-ch:
		if got.From != "worker" || got.ConvID != "C1" {
			t.Fatalf("got %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("awaiter never received the dispatched message")
	}
}

func TestAwaiter_NoMatchGoesToFallback(t *testing.T) {
	a := New()
	// No registration — every dispatch should land in fallback.
	a.Dispatch(&hive.PeerMessage{From: "writer", ConvID: "C2", Payload: []byte(`{}`)})

	select {
	case got := <-a.Fallback():
		if got.From != "writer" {
			t.Fatalf("fallback got wrong msg: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("fallback channel never received the un-matched message")
	}
}

func TestAwaiter_KeyMismatchGoesToFallback(t *testing.T) {
	a := New()
	// Register expecting reply from "writer"/conv-A; arrival is from
	// "writer"/conv-B (different conv) — should fall through to fallback,
	// not the awaiter.
	awaitCh, cancel := a.Register("writer", "conv-A")
	defer cancel()

	a.Dispatch(&hive.PeerMessage{From: "writer", ConvID: "conv-B", Payload: []byte(`{}`)})

	select {
	case <-awaitCh:
		t.Fatal("awaiter wrongly received message for different conv")
	case got := <-a.Fallback():
		if got.ConvID != "conv-B" {
			t.Fatalf("fallback got wrong msg: %+v", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected fallback delivery within 500ms")
	}
}

func TestAwaiter_CancelUnregisters(t *testing.T) {
	a := New()
	_, cancel := a.Register("worker", "C1")
	cancel()

	// Subsequent dispatch with the same key must NOT find an awaiter.
	delivered := a.Dispatch(&hive.PeerMessage{From: "worker", ConvID: "C1"})
	if delivered {
		t.Fatal("dispatch found a cancelled awaiter")
	}
}

func TestAwaiter_DoubleRegisterClosesPrior(t *testing.T) {
	a := New()
	ch1, _ := a.Register("worker", "C1")
	// Register again with same key — should close ch1 (signalling cancel
	// to whoever was reading it) and replace with a fresh channel.
	ch2, cancel := a.Register("worker", "C1")
	defer cancel()

	select {
	case _, ok := <-ch1:
		if ok {
			t.Fatal("expected closed channel, got value")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("first awaiter's channel was not closed")
	}

	go a.Dispatch(&hive.PeerMessage{From: "worker", ConvID: "C1", Payload: []byte(`"new"`)})
	select {
	case got := <-ch2:
		if got == nil {
			t.Fatal("second awaiter got nil")
		}
	case <-time.After(time.Second):
		t.Fatal("second awaiter didn't receive new dispatch")
	}
}

func TestAwaiter_ConcurrentDispatch(t *testing.T) {
	a := New()
	const N = 50
	var wg sync.WaitGroup
	results := make(chan bool, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			conv := "conv-" + itoa(i)
			ch, cancel := a.Register("worker", conv)
			defer cancel()
			go a.Dispatch(&hive.PeerMessage{From: "worker", ConvID: conv})
			select {
			case <-ch:
				results <- true
			case <-time.After(time.Second):
				results <- false
			}
		}(i)
	}
	wg.Wait()
	close(results)
	got := 0
	for ok := range results {
		if ok {
			got++
		}
	}
	if got != N {
		t.Fatalf("only %d of %d concurrent awaits delivered", got, N)
	}
}

// itoa avoids strconv import in this small test file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
