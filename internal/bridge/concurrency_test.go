package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConcurrentLocalCallsOutOfOrderAndPublished(t *testing.T) {
	m, up, down := readyMux(t)
	const count = 40
	events, cancelEvents := m.Subscribe(count)
	defer cancelEvents()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := m.Call(ctx, "test/echo", map[string]int{"value": i})
			if err != nil || string(result) != fmt.Sprintf(`{"value":%d,"large":9007199254740993}`, i) {
				t.Errorf("call %d: result=%s err=%v", i, result, err)
			}
		}()
	}
	type request struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Value int `json:"value"`
		} `json:"params"`
	}
	requests := make([]request, count)
	seen := make(map[string]bool)
	for i := range count {
		frame := nextFrame(t, up)
		if err := json.Unmarshal(frame, &requests[i]); err != nil {
			t.Fatal(err)
		}
		if seen[string(requests[i].ID)] {
			t.Fatal("concurrent local request IDs collided")
		}
		seen[string(requests[i].ID)] = true
	}
	before := down.text()
	for i := count - 1; i >= 0; i-- {
		mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":{"value":%d,"large":9007199254740993},"extra":true}`, requests[i].ID, requests[i].Params.Value))
	}
	wg.Wait()
	if down.text() != before || m.Status().PendingLocal != 0 {
		t.Fatal("local results leaked to Desktop or retained pending calls")
	}
	for i := range count {
		if event := <-events; event.Sequence != uint64(i+2) || !strings.Contains(string(event.Message), `"extra":true`) {
			t.Fatalf("local response event = %+v", event)
		}
	}
	// A duplicate success result also stays off the Desktop connection.
	mustServer(t, m, fmt.Sprintf(`{"id":%s,"result":true}`, requests[0].ID))
	if down.text() != before {
		t.Fatal("duplicate local result reached Desktop")
	}
}

func TestDesktopAndCompanionApprovalRace(t *testing.T) {
	m, up, down := readyMux(t)
	for i := range 50 {
		id := fmt.Sprintf(`"request-%d"`, i)
		mustServer(t, m, fmt.Sprintf(`{"id":%s,"method":"approval"}`, id))
		nextFrame(t, down)
		before := up.text()
		start := make(chan struct{})
		errs := make(chan error, 2)
		go func() {
			<-start
			errs <- m.FromDesktop([]byte(fmt.Sprintf(`{"id":%s,"result":{"decision":"decline"}}`, id)))
		}()
		go func() {
			<-start
			errs <- m.Respond(json.RawMessage(id), map[string]string{"decision": "accept"})
		}()
		close(start)
		for range 2 {
			if err := <-errs; err != nil && !errors.Is(err, ErrNoPendingRequest) {
				t.Fatal(err)
			}
		}
		if writes := strings.TrimPrefix(up.text(), before); strings.Count(writes, "\n") != 1 {
			t.Fatalf("approval race produced %q", writes)
		}
	}
}

func TestConcurrentWritesRemainWholeWithShortWriter(t *testing.T) {
	up := &shortWriter{}
	m := New(up, io.Discard)
	defer m.Close(nil)
	const count = 100
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := m.FromDesktop([]byte(fmt.Sprintf(`{"method":"notification","params":{"i":%d}}`, i))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	seen := make(map[int]bool)
	err := ReadFrames(strings.NewReader(up.String()), 0, func(frame []byte) error {
		var notification struct {
			Params struct {
				I int `json:"i"`
			} `json:"params"`
		}
		if err := json.Unmarshal(frame, &notification); err != nil {
			return err
		}
		seen[notification.Params.I] = true
		return nil
	})
	if err != nil || len(seen) != count {
		t.Fatalf("short/concurrent writes interleaved: count=%d err=%v", len(seen), err)
	}
}

func TestSubscriberMemoryBoundAndBudgetReclamation(t *testing.T) {
	m := New(io.Discard, io.Discard)
	defer m.Close(nil)
	events, cancel := m.Subscribe(20)
	defer cancel()
	frame := `{"method":"large","params":"` + strings.Repeat("x", 1<<20) + `"}`
	// Draining between writes must reclaim the byte budget, not accumulate it
	// for the lifetime of a subscription.
	for range 20 {
		mustServer(t, m, frame)
		if _, ok := <-events; !ok {
			t.Fatal("drained subscriber incorrectly exceeded its byte budget")
		}
	}
	for range 20 {
		mustServer(t, m, frame)
	}
	count := 0
	for range events {
		count++
	}
	if count != 15 || m.Status().Subscribers != 0 {
		t.Fatalf("unread byte budget not enforced: count=%d subscribers=%d", count, m.Status().Subscribers)
	}
}

func TestIDRewritePreservesEscapedFieldNames(t *testing.T) {
	up, down := newCapture(), newCapture()
	m := New(up, down)
	defer m.Close(nil)
	escapedKey := `"` + `\` + `u0069d"`
	escapedID := `"` + `\` + `u0061b"`
	mustDesktop(t, m, ` {`+escapedKey+` : `+escapedID+`, "method":"read", "trace":"\/"} `)
	wire := nextFrame(t, up)
	mapped := frameID(t, wire)
	if !strings.Contains(string(wire), escapedKey) {
		t.Fatal("request ID field spelling changed")
	}
	mustServer(t, m, ` {`+escapedKey+`: `+string(mapped)+`, "result":null, "extra":"a"} `)
	if got := string(nextFrame(t, down)); got != ` {`+escapedKey+`: `+escapedID+`, "result":null, "extra":"a"} ` {
		t.Fatalf("escaped field spelling changed: %s", got)
	}
}
