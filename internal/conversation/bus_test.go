package conversation

import (
	"sync"
	"testing"
	"time"
)

func TestBusPublishSubscribe(t *testing.T) {
	b := NewBus()
	ch, cancel := b.Subscribe("room-A")
	defer cancel()

	b.Publish(Event{Type: EventConvCreated, RoomID: "room-A", ConvID: "c1"})

	select {
	case e := <-ch:
		if e.Type != EventConvCreated || e.ConvID != "c1" {
			t.Errorf("got %+v", e)
		}
		if e.TS.IsZero() {
			t.Error("Bus should default TS")
		}
	case <-time.After(time.Second):
		t.Fatal("no event received")
	}
}

func TestBusFilterByRoom(t *testing.T) {
	b := NewBus()
	chA, cancelA := b.Subscribe("room-A")
	defer cancelA()
	chB, cancelB := b.Subscribe("room-B")
	defer cancelB()

	b.Publish(Event{Type: EventConvCreated, RoomID: "room-A", ConvID: "c1"})

	select {
	case e := <-chA:
		if e.RoomID != "room-A" {
			t.Errorf("A got %s", e.RoomID)
		}
	case <-time.After(time.Second):
		t.Fatal("A: no event")
	}

	select {
	case e := <-chB:
		t.Errorf("B should not receive room-A event, got %+v", e)
	case <-time.After(50 * time.Millisecond):
		// expected — no event
	}
}

func TestBusCancelClosesChannel(t *testing.T) {
	b := NewBus()
	ch, cancel := b.Subscribe("room-A")

	if got := b.SubscriberCount("room-A"); got != 1 {
		t.Errorf("count=%d want 1", got)
	}

	cancel()
	if got := b.SubscriberCount("room-A"); got != 0 {
		t.Errorf("after cancel count=%d want 0", got)
	}

	// Channel should be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected closed channel after cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("channel not closed after cancel")
	}

	// Cancel is idempotent.
	cancel()
}

func TestBusDropsOnSlowSubscriber(t *testing.T) {
	b := NewBus()
	_, cancel := b.Subscribe("room-A") // never read
	defer cancel()

	// Flood far past the buffer; publisher should not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < busSubBuffer*4; i++ {
			b.Publish(Event{Type: EventConvMessage, RoomID: "room-A"})
		}
		close(done)
	}()
	select {
	case <-done:
		// publisher didn't deadlock — good
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
}

func TestBusConcurrentSubscribePublish(t *testing.T) {
	b := NewBus()
	const N = 20
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, cancel := b.Subscribe("room-A")
			defer cancel()
			// Drain at least one event then exit.
			select {
			case <-ch:
			case <-time.After(2 * time.Second):
			}
		}()
	}
	// Publisher running concurrently.
	for i := 0; i < 200; i++ {
		b.Publish(Event{Type: EventConvCreated, RoomID: "room-A"})
		time.Sleep(time.Millisecond)
	}
	wg.Wait()
	// All subscribers should have unregistered.
	if got := b.SubscriberCount("room-A"); got != 0 {
		t.Errorf("leaked subscribers: %d", got)
	}
}
